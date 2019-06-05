package minecraft

import (
	"bytes"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/sandertv/gophertunnel/minecraft/protocol"
	"github.com/sandertv/gophertunnel/minecraft/protocol/login"
	"github.com/sandertv/gophertunnel/minecraft/protocol/login/jwt"
	"github.com/sandertv/gophertunnel/minecraft/protocol/packet"
	"github.com/sandertv/gophertunnel/minecraft/resource"
	"io"
	"log"
	"net"
	"strings"
	"sync"
	"time"
)

// Conn represents a Minecraft (Bedrock Edition) connection over a specific net.Conn transport layer. Its
// methods (Read, Write etc.) are safe to be called from multiple goroutines simultaneously.
type Conn struct {
	conn net.Conn
	log  *log.Logger

	pool    packet.Pool
	encoder *packet.Encoder
	decoder *packet.Decoder

	identityData login.IdentityData
	clientData   login.ClientData

	// privateKey is the private key of this end of the connection. Each connection, regardless of which side
	// the connection is on, server or client, has a unique private key generated.
	privateKey *ecdsa.PrivateKey
	// salt is a 16 byte long randomly generated byte slice which is only used if the Conn is a server sided
	// connection. It is otherwise left unused.
	salt []byte

	// packets is a channel of byte slices containing serialised packets that are coming in from the other
	// side of the connection.
	packets      chan []byte
	readDeadline <-chan time.Time

	sendMutex sync.Mutex
	// bufferedSend is a slice of byte slices containing packets that are 'written'. They are buffered until
	// they are sent each 20th of a second.
	bufferedSend [][]byte

	// loggedIn is a bool indicating if the connection was logged in. It is set to true after the entire login
	// sequence is completed.
	loggedIn bool
	// expectedIDs is a slice of packet identifiers that are next expected to arrive, until the connection is
	// logged in.
	expectedIDs []uint32

	// resourcePacks is a slice of resource packs that the listener may hold. Each client will be asked to
	// download these resource packs upon joining.
	resourcePacks []*resource.Pack
	// texturePacksRequired specifies if clients that join must accept the texture pack in order for them to
	// be able to join the server. If they don't accept, they can only leave the server.
	texturePacksRequired bool

	packQueue *resourcePackQueue

	// packetFunc is an optional function passed to a Dial() call. If set, each packet read from and written
	// to this connection will call this function.
	packetFunc func(header packet.Header, payload []byte, src, dst net.Addr)

	connected chan bool
	close     chan bool
}

// newConn creates a new Minecraft connection for the net.Conn passed, reading and writing compressed
// Minecraft packets to that net.Conn.
// newConn accepts a private key which will be used to identify the connection. If a nil key is passed, the
// key is generated.
func newConn(conn net.Conn, key *ecdsa.PrivateKey, log *log.Logger) *Conn {
	if key == nil {
		// If not key is passed, we generate one in this function and use it instead.
		key, _ = ecdsa.GenerateKey(elliptic.P384(), rand.Reader)
	}
	c := &Conn{
		conn:      conn,
		encoder:   packet.NewEncoder(conn),
		decoder:   packet.NewDecoder(conn),
		pool:      packet.NewPool(),
		packets:   make(chan []byte, 32),
		connected: make(chan bool),
		close:     make(chan bool, 1),
		// By default we set this to the login packet, but a client will have to set the play status packet's
		// ID as the first expected one.
		expectedIDs: []uint32{packet.IDLogin},
		privateKey:  key,
		salt:        make([]byte, 16),
		log:         log,
	}
	_, _ = rand.Read(c.salt)

	go func() {
		ticker := time.NewTicker(time.Second / 20)
		defer ticker.Stop()
		for {
			select {
			case <-ticker.C:
				if err := c.Flush(); err != nil {
					return
				}
			case <-c.close:
				// Break out of the goroutine and propagate the close signal again.
				c.close <- true
				return
			}
		}
	}()
	return c
}

// IdentityData returns the identity data of the connection. It holds the UUID, XUID and username of the
// connected client.
func (conn *Conn) IdentityData() login.IdentityData {
	return conn.identityData
}

// ClientData returns the client data the client connected with. Note that this client data may be changed
// during the session, so the data should only be used directly after connection, and should be updated after
// that by the caller.
func (conn *Conn) ClientData() login.ClientData {
	return conn.clientData
}

// WritePacket encodes the packet passed and writes it to the Conn. The encoded data is buffered until the
// next 20th of a second, after which the data is flushed and sent over the connection.
func (conn *Conn) WritePacket(pk packet.Packet) error {
	header := &packet.Header{PacketID: pk.ID()}
	buffer := bytes.NewBuffer(make([]byte, 0, 5))
	if err := header.Write(buffer); err != nil {
		return fmt.Errorf("error writing packet header: %v", err)
	}
	// Record the length of the header so we can filter it out for the packet func.
	headerLen := buffer.Len()

	pk.Marshal(buffer)
	if conn.packetFunc != nil {
		// The packet func was set, so we call it.
		conn.packetFunc(*header, buffer.Bytes()[headerLen:], conn.LocalAddr(), conn.RemoteAddr())
	}
	_, err := conn.Write(buffer.Bytes())
	return err
}

// ReadPacket reads a packet from the Conn, depending on the packet ID that is found in front of the packet
// data. If a read deadline is set, an error is returned if the deadline is reached before any packet is
// received.
// The packet received must not be held until the next packet is read using ReadPacket(). If the same type of
// packet is read, the previous one will be invalidated.
//
// If the packet read was not implemented, a *packet.Unknown is returned, containing the raw payload of the
// packet read.
func (conn *Conn) ReadPacket() (pk packet.Packet, err error) {
read:
	select {
	case data := <-conn.packets:
		buf := bytes.NewBuffer(data)
		header := &packet.Header{}
		if err := header.Read(buf); err != nil {
			// We don't return this as an error as it's not in the hand of the user to control this. Instead,
			// we return to reading a new packet.
			conn.log.Printf("error reading packet header: %v", err)
			goto read
		}
		if conn.packetFunc != nil {
			// The packet func was set, so we call it.
			conn.packetFunc(*header, buf.Bytes(), conn.RemoteAddr(), conn.LocalAddr())
		}
		// Attempt to fetch the packet with the right packet ID from the pool.
		pk, ok := conn.pool[header.PacketID]
		if !ok {
			// We haven't implemented this packet ID, so we return an unknown packet which could be used by
			// the reader.
			pk = &packet.Unknown{PacketID: header.PacketID}
		}
		if err := pk.Unmarshal(buf); err != nil {
			// We don't return this as an error as it's not in the hand of the user to control this. Instead,
			// we return to reading a new packet.
			conn.log.Printf("error decoding packet %T: %v", pk, err)
			goto read
		}
		if buf.Len() != 0 {
			conn.log.Printf("%v unread bytes left in packet %T%v: %v (full payload: %v)\n", buf.Len(), pk, fmt.Sprintf("%+v", pk)[1:], hex.EncodeToString(buf.Bytes()), hex.EncodeToString(data))
		}
		// Unmarshal the bytes into the packet and return the error.
		return pk, nil
	case <-conn.readDeadline:
		return nil, fmt.Errorf("error reading packet: read timeout")
	case <-conn.close:
		conn.close <- true
		return nil, fmt.Errorf("error reading packet: connection closed")
	}
}

// ResourcePacks returns a slice of all resource packs the connection holds. For a Conn obtained using a
// Listener, this holds all resource packs set to the Listener. For a Conn obtained using Dial, the resource
// packs include all packs sent by the server connected to.
func (conn *Conn) ResourcePacks() []*resource.Pack {
	return conn.resourcePacks
}

// Write writes a slice of serialised packet data to the Conn. The data is buffered until the next 20th of a
// tick, after which it is flushed to the connection. Write returns the amount of bytes written n.
func (conn *Conn) Write(b []byte) (n int, err error) {
	conn.sendMutex.Lock()
	defer conn.sendMutex.Unlock()

	conn.bufferedSend = append(conn.bufferedSend, b)
	return len(b), nil
}

// Read reads a packet from the connection into the byte slice passed, provided the byte slice is big enough
// to carry the full packet.
// It is recommended to use ReadPacket() rather than Read() in cases where reading is done directly.
func (conn *Conn) Read(b []byte) (n int, err error) {
	select {
	case data := <-conn.packets:
		if len(b) < len(data) {
			return 0, fmt.Errorf("error reading data: A message sent on a Minecraft socket was larger than the buffer used to receive the message into")
		}
		return copy(b, data), nil
	case <-conn.readDeadline:
		return 0, fmt.Errorf("error reading packet: read timeout")
	case <-conn.close:
		conn.close <- true
		return 0, fmt.Errorf("error reading packet: connection closed")
	}
}

// Flush flushes the packets currently buffered by the connections to the underlying net.Conn, so that they
// are directly sent.
func (conn *Conn) Flush() error {
	conn.sendMutex.Lock()
	defer conn.sendMutex.Unlock()

	if len(conn.bufferedSend) > 0 {
		if err := conn.encoder.Encode(conn.bufferedSend); err != nil {
			return fmt.Errorf("error encoding packet batch: %v", err)
		}
		// Reset the send slice so that we don't accidentally send the same packets.
		conn.bufferedSend = nil
	}
	return nil
}

// Close closes the Conn and its underlying connection. Before closing, it also calls Flush() so that any
// packets currently pending are sent out.
func (conn *Conn) Close() error {
	if len(conn.close) != 0 {
		// The connection was already closed, no need to do anything.
		return nil
	}
	_ = conn.Flush()
	conn.close <- true
	return conn.conn.Close()
}

// LocalAddr returns the local address of the underlying connection.
func (conn *Conn) LocalAddr() net.Addr {
	return conn.conn.LocalAddr()
}

// RemoteAddr returns the remote address of the underlying connection.
func (conn *Conn) RemoteAddr() net.Addr {
	return conn.conn.RemoteAddr()
}

// SetDeadline sets the read and write deadline of the connection. It is equivalent to calling SetReadDeadline
// and SetWriteDeadline at the same time.
func (conn *Conn) SetDeadline(t time.Time) error {
	return conn.SetReadDeadline(t)
}

// SetReadDeadline sets the read deadline of the Conn to the time passed. The time must be after time.Now().
// Passing an empty time.Time to the method (time.Time{}) results in the read deadline being cleared.
func (conn *Conn) SetReadDeadline(t time.Time) error {
	if t.Before(time.Now()) {
		return fmt.Errorf("error setting read deadline: time passed is before time.Now()")
	}
	empty := time.Time{}
	if t == empty {
		// Empty time, so we just set the time to some crazy high value to ensure the read deadline is never
		// actually reached.
		conn.readDeadline = time.After(time.Hour * 1000000)
	} else {
		conn.readDeadline = time.After(t.Sub(time.Now()))
	}
	return nil
}

// SetWriteDeadline is a stub function to implement net.Conn. It has no functionality.
func (conn *Conn) SetWriteDeadline(t time.Time) error {
	return nil
}

// handleIncoming handles an incoming serialised packet from the underlying connection. If the connection is
// not yet logged in, the packet is immediately read and processed.
func (conn *Conn) handleIncoming(data []byte) error {
	conn.packets <- data
	if !conn.loggedIn {
		pk, err := conn.ReadPacket()
		if err != nil {
			return err
		}
		found := false
		for _, id := range conn.expectedIDs {
			if id == pk.ID() || pk.ID() == packet.IDDisconnect {
				// If the packet was expected, we set found to true and handle it. If not, we skip it and
				// ignore it eventually.
				found = true
				break
			}
		}
		if !found {
			// This is not the packet we expected next in the login sequence. We just ignore it as it might
			// be a packet such as a movement that was simply sent too early.
			return nil
		}
		switch pk := pk.(type) {
		// Internal packets destined for the server.
		case *packet.Login:
			return conn.handleLogin(pk)
		case *packet.ClientToServerHandshake:
			return conn.handleClientToServerHandshake(pk)
		case *packet.ResourcePackClientResponse:
			return conn.handleResourcePackClientResponse(pk)
		case *packet.ResourcePackChunkRequest:
			return conn.handleResourcePackChunkRequest(pk)
		case *packet.RequestChunkRadius:
			// TODO

		// Internal packets destined for the client.
		case *packet.ServerToClientHandshake:
			return conn.handleServerToClientHandshake(pk)
		case *packet.PlayStatus:
			return conn.handlePlayStatus(pk)
		case *packet.ResourcePacksInfo:
			return conn.handleResourcePacksInfo(pk)
		case *packet.ResourcePackDataInfo:
			return conn.handleResourcePackDataInfo(pk)
		case *packet.ResourcePackChunkData:
			return conn.handleResourcePackChunkData(pk)
		case *packet.ResourcePackStack:
			return conn.handleResourcePackStack(pk)
		case *packet.StartGame:
			// TODO

		case *packet.Disconnect:
			_ = conn.Close()
			return errors.New("Disconnected: " + pk.Message)
		}
	}
	return nil
}

// handleLogin handles an incoming login packet. It verifies an decodes the login request found in the packet
// and returns an error if it couldn't be done successfully.
func (conn *Conn) handleLogin(pk *packet.Login) error {
	// The next expected packet is a response from the client to the handshake.
	conn.expect(packet.IDClientToServerHandshake)

	if pk.ClientProtocol != protocol.CurrentProtocol {
		// By default we assume the client is outdated.
		status := packet.PlayStatusLoginFailedClient
		if pk.ClientProtocol > protocol.CurrentProtocol {
			// The server is outdated in this case, so we have to change the status we send.
			status = packet.PlayStatusLoginFailedServer
		}
		_ = conn.WritePacket(&packet.PlayStatus{Status: status})
		return conn.Close()
	}

	publicKey, authenticated, err := login.Verify(pk.ConnectionRequest)
	if err != nil {
		return fmt.Errorf("error verifying login request: %v", err)
	}
	if !authenticated {
		return fmt.Errorf("connection %v was not authenticated to XBOX Live", conn.RemoteAddr())
	}
	conn.identityData, conn.clientData, err = login.Decode(pk.ConnectionRequest)
	if err != nil {
		return fmt.Errorf("error decoding login request: %v", err)
	}
	// First validate the identity data and the client data to ensure we're working with valid data. Mojang
	// might change this data, or some custom client might fiddle with the data, so we can never be too sure.
	if err := conn.identityData.Validate(); err != nil {
		return fmt.Errorf("invalid identity data: %v", err)
	}
	if err := conn.clientData.Validate(); err != nil {
		return fmt.Errorf("invalid client data: %v", err)
	}
	if err := conn.enableEncryption(publicKey); err != nil {
		return fmt.Errorf("error enabling encryption: %v", err)
	}
	return nil
}

// handleClientToServerHandshake handles an incoming ClientToServerHandshake packet.
func (conn *Conn) handleClientToServerHandshake(*packet.ClientToServerHandshake) error {
	// The next expected packet is a resource pack client response.
	conn.expect(packet.IDResourcePackClientResponse)

	if err := conn.WritePacket(&packet.PlayStatus{Status: packet.PlayStatusLoginSuccess}); err != nil {
		return fmt.Errorf("error sending play status login success: %v", err)
	}
	pk := &packet.ResourcePacksInfo{TexturePackRequired: conn.texturePacksRequired}
	for _, pack := range conn.resourcePacks {
		resourcePack := packet.ResourcePack{UUID: pack.UUID(), Version: pack.Version(), Size: int64(pack.Len())}
		if pack.HasScripts() {
			// One of the resource packs has scripts, so we set HasScripts in the packet to true.
			pk.HasScripts = true
			resourcePack.HasScripts = true
		}
		// If it has behaviours, add it to the behaviour pack list. If not, we add it to the texture packs
		// list.
		if pack.HasBehaviours() {
			pk.BehaviourPacks = append(pk.BehaviourPacks, resourcePack)
			continue
		}
		pk.TexturePacks = append(pk.TexturePacks, resourcePack)
	}
	// Finally we send the packet after the play status.
	if err := conn.WritePacket(pk); err != nil {
		return fmt.Errorf("error sending resource packs info: %v", err)
	}
	return nil
}

// handleServerToClientHandshake handles an incoming ServerToClientHandshake packet. It initialises encryption
// on the client side of the connection, using the hash and the public key from the server exposed in the
// packet.
func (conn *Conn) handleServerToClientHandshake(pk *packet.ServerToClientHandshake) error {
	headerData, err := jwt.HeaderFrom(pk.JWT)
	if err != nil {
		return fmt.Errorf("error reading ServerToClientHandshake JWT header: %v", err)
	}
	header := &jwt.Header{}
	if err := json.Unmarshal(headerData, header); err != nil {
		return fmt.Errorf("error parsing ServerToClientHandshake JWT header JSON: %v", err)
	}
	if !jwt.AllowedAlg(header.Algorithm) {
		return fmt.Errorf("ServerToClientHandshake JWT header had unexpected alg: expected %v, got %v", "ES384", header.Algorithm)
	}
	// First parse the public pubKey, so that we can use it to verify the entire JWT afterwards. The JWT is self-
	// signed by the server.
	pubKey := &ecdsa.PublicKey{}
	if err := jwt.ParsePublicKey(header.X5U, pubKey); err != nil {
		return fmt.Errorf("error parsing ServerToClientHandshake header x5u public pubKey: %v", err)
	}
	if _, err := jwt.Verify(pk.JWT, pubKey, false); err != nil {
		return fmt.Errorf("error verifying ServerToClientHandshake JWT: %v", err)
	}
	// We already know the JWT is valid as we verified it, so no need to error check.
	body, _ := jwt.Payload(pk.JWT)
	m := make(map[string]string)
	if err := json.Unmarshal(body, &m); err != nil {
		return fmt.Errorf("error parsing ServerToClientHandshake JWT payload JSON: %v", err)
	}
	b64Salt, ok := m["salt"]
	if !ok {
		return fmt.Errorf("ServerToClientHandshake JWT payload contained no 'salt'")
	}
	// Some (faulty) JWT implementations use padded base64, whereas it should be raw. We trim this off.
	b64Salt = strings.TrimRight(b64Salt, "=")
	salt, err := base64.RawStdEncoding.DecodeString(b64Salt)
	if err != nil {
		return fmt.Errorf("error base64 decoding ServerToClientHandshake salt: %v", err)
	}

	x, _ := pubKey.Curve.ScalarMult(pubKey.X, pubKey.Y, conn.privateKey.D.Bytes())
	sharedSecret := x.Bytes()
	keyBytes := sha256.Sum256(append(salt, sharedSecret...))

	// Finally we enable encryption for the encoder and decoder using the secret pubKey bytes we produced.
	conn.encoder.EnableEncryption(keyBytes)
	conn.decoder.EnableEncryption(keyBytes)

	// We write a ClientToServerHandshake packet (which has no payload) as a response.
	return conn.WritePacket(&packet.ClientToServerHandshake{})
}

// handleResourcePacksInfo handles a ResourcePacksInfo packet sent by the server. The client responds by
// sending the packs it needs downloaded.
func (conn *Conn) handleResourcePacksInfo(pk *packet.ResourcePacksInfo) error {
	// First create a new resource pack queue with the information in the packet so we can download them
	// properly later.
	conn.packQueue = &resourcePackQueue{
		packAmount:       len(pk.TexturePacks) + len(pk.BehaviourPacks),
		downloadingPacks: make(map[string]downloadingPack),
		awaitingPacks:    make(map[string]*downloadingPack),
	}

	packsToDownload := make([]string, 0, len(pk.TexturePacks)+len(pk.BehaviourPacks))
	for _, pack := range pk.TexturePacks {
		// This UUID_Version is a hack Mojang put in place.
		packsToDownload = append(packsToDownload, pack.UUID+"_"+pack.Version)
		conn.packQueue.downloadingPacks[pack.UUID] = downloadingPack{size: pack.Size, buf: bytes.NewBuffer(make([]byte, 0, pack.Size)), newFrag: make(chan []byte)}
	}
	for _, pack := range pk.BehaviourPacks {
		// This UUID_Version is a hack Mojang put in place.
		packsToDownload = append(packsToDownload, pack.UUID+"_"+pack.Version)
		conn.packQueue.downloadingPacks[pack.UUID] = downloadingPack{size: pack.Size, buf: bytes.NewBuffer(make([]byte, 0, pack.Size)), newFrag: make(chan []byte)}
	}
	if len(packsToDownload) != 0 {
		conn.expect(packet.IDResourcePackDataInfo, packet.IDResourcePackChunkData)
		return conn.WritePacket(&packet.ResourcePackClientResponse{
			Response:        packet.PackResponseSendPacks,
			PacksToDownload: packsToDownload,
		})
	}
	conn.expect(packet.IDResourcePackStack)
	return conn.WritePacket(&packet.ResourcePackClientResponse{Response: packet.PackResponseAllPacksDownloaded})
}

// handleResourcePackStack handles a ResourcePackStack packet sent by the server. The stack defines the order
// that resource packs are applied in.
func (conn *Conn) handleResourcePackStack(pk *packet.ResourcePackStack) error {
	// We currently don't apply resource packs in any way, so instead we just check if all resource packs in
	// the stacks are also downloaded.
	for _, pack := range pk.TexturePacks {
		if !conn.hasPack(pack.UUID, pack.Version, false) {
			return fmt.Errorf("texture pack {uuid=%v, version=%v} not downloaded", pack.UUID, pack.Version)
		}
	}
	for _, pack := range pk.BehaviourPacks {
		if !conn.hasPack(pack.UUID, pack.Version, true) {
			return fmt.Errorf("behaviour pack {uuid=%v, version=%v} not downloaded", pack.UUID, pack.Version)
		}
	}
	conn.connected <- true
	conn.loggedIn = true
	return conn.WritePacket(&packet.ResourcePackClientResponse{Response: packet.PackResponseCompleted})
}

// hasPack checks if the connection has a resource pack downloaded with the UUID and version passed, provided
// the pack either has or does not have behaviours in it.
func (conn *Conn) hasPack(uuid string, version string, hasBehaviours bool) bool {
	for _, pack := range conn.resourcePacks {
		if pack.UUID() == uuid && pack.Version() == version && pack.HasBehaviours() == hasBehaviours {
			return true
		}
	}
	return false
}

// packChunkSize is the size of a single chunk of data from a resource pack: 512 kB or 0.5 MB
const packChunkSize = 1024 * 512

// handleResourcePackClientResponse handles an incoming resource pack client response packet. The packet is
// handled differently depending on the response.
func (conn *Conn) handleResourcePackClientResponse(pk *packet.ResourcePackClientResponse) error {
	switch pk.Response {
	case packet.PackResponseRefused:
		// Even though this response is never sent, we handle it appropriately in case it is changed to work
		// correctly again.
		return conn.Close()
	case packet.PackResponseSendPacks:
		packs := pk.PacksToDownload
		conn.packQueue = &resourcePackQueue{packs: conn.resourcePacks}
		if err := conn.packQueue.Request(packs); err != nil {
			return fmt.Errorf("error looking up resource packs to download: %v", err)
		}
		// Proceed with the first resource pack download. We run all downloads in sequence rather than in
		// parallel, as it's less prone to packet loss.
		if err := conn.nextResourcePackDownload(); err != nil {
			return err
		}
	case packet.PackResponseAllPacksDownloaded:
		pk := &packet.ResourcePackStack{TexturePackRequired: conn.texturePacksRequired}
		for _, pack := range conn.resourcePacks {
			resourcePack := packet.ResourcePack{UUID: pack.UUID(), Version: pack.Version()}
			// If it has behaviours, add it to the behaviour pack list. If not, we add it to the texture packs
			// list.
			if pack.HasBehaviours() {
				pk.BehaviourPacks = append(pk.BehaviourPacks, resourcePack)
				continue
			}
			pk.TexturePacks = append(pk.TexturePacks, resourcePack)
		}
		if err := conn.WritePacket(pk); err != nil {
			return fmt.Errorf("error writing resource pack stack packet: %v", err)
		}
	case packet.PackResponseCompleted:
		// This is as far as we can go in terms of covering up the login sequence. The next packet is the
		// StartGame packet, which includes far too many fields related to the world which we simply cannot
		// fill out in advance.
		conn.loggedIn = true
	default:
		return fmt.Errorf("unknown resource pack client response: %v", pk.Response)
	}
	return nil
}

// nextResourcePackDownload moves to the next resource pack to download and sends a resource pack data info
// packet with information about it.
func (conn *Conn) nextResourcePackDownload() error {
	pk, ok := conn.packQueue.NextPack()
	if !ok {
		return fmt.Errorf("no resource packs to download")
	}
	if err := conn.WritePacket(pk); err != nil {
		return fmt.Errorf("error sending resource pack data info packet: %v", err)
	}
	// Set the next expected packet to ResourcePackChunkRequest packets.
	conn.expect(packet.IDResourcePackChunkRequest)
	return nil
}

// handleResourcePackDataInfo handles a resource pack data info packet, which initiates the downloading of the
// pack by the client.
func (conn *Conn) handleResourcePackDataInfo(pk *packet.ResourcePackDataInfo) error {
	uuid := pk.UUID
	chunkCount := pk.ChunkCount
	downloadingPack, ok := conn.packQueue.downloadingPacks[uuid]
	if !ok {
		// We either already downloaded the pack or we got sent an invalid UUID, that did not match any pack
		// sent in the ResourcePacksInfo packet.
		return fmt.Errorf("unknown pack to download with UUID %v", uuid)
	}
	if downloadingPack.size != pk.Size {
		// Size mismatch: The ResourcePacksInfo packet had a size for the pack that did not match with the
		// size sent here.
		return fmt.Errorf("pack %v had a different size in the ResourcePacksInfo packet than the ResourcePackDataInfo packet", uuid)
	}

	// Remove the resource pack from the downloading packs and add it to the awaiting packets.
	delete(conn.packQueue.downloadingPacks, uuid)
	conn.packQueue.awaitingPacks[uuid] = &downloadingPack

	downloadingPack.chunkSize = pk.DataChunkSize
	go func() {
		for i := int32(0); i < chunkCount; i++ {
			_ = conn.WritePacket(&packet.ResourcePackChunkRequest{
				UUID:       uuid,
				ChunkIndex: i,
			})
			// Write the fragment to the full buffer of the downloading resource pack.
			_, _ = downloadingPack.buf.Write(<-downloadingPack.newFrag)
		}
		if downloadingPack.buf.Len() != int(downloadingPack.size) {
			conn.log.Printf("incorrect resource pack size: expected %v, but got %v\n", downloadingPack.size, downloadingPack.buf.Len())
			return
		}
		// First parse the resource pack from the total byte buffer we obtained.
		pack, err := resource.FromBytes(downloadingPack.buf.Bytes())
		if err != nil {
			conn.log.Printf("invalid full resource pack data for UUID %v: %v\n", uuid, err)
			return
		}
		conn.packQueue.packAmount--
		// Finally we add the resource to the resource packs slice.
		conn.resourcePacks = append(conn.resourcePacks, pack)
		if conn.packQueue.packAmount == 0 {
			conn.expect(packet.IDResourcePackStack)
			_ = conn.WritePacket(&packet.ResourcePackClientResponse{Response: packet.PackResponseAllPacksDownloaded})
		}
	}()
	return nil
}

// handleResourcePackChunkData handles a resource pack chunk data packet, which holds a fragment of a resource
// pack that is being downloaded.
func (conn *Conn) handleResourcePackChunkData(pk *packet.ResourcePackChunkData) error {
	downloadingPack, ok := conn.packQueue.awaitingPacks[pk.UUID]
	if !ok {
		// We haven't received a ResourcePackDataInfo packet from the server, so we can't use this data to
		// download a resource pack.
		return fmt.Errorf("resource pack chunk data for resource pack that was not being downloaded")
	}
	lastData := downloadingPack.buf.Len()+int(downloadingPack.chunkSize) >= int(downloadingPack.size)
	if !lastData && int32(len(pk.Data)) != downloadingPack.chunkSize {
		// The chunk data didn't have the full size and wasn't the last data to be sent for the resource pack,
		// meaning we got too little data.
		return fmt.Errorf("resource pack chunk data had a length of %v, but expected %v", len(pk.Data), downloadingPack.chunkSize)
	}
	if pk.ChunkIndex != downloadingPack.expectedIndex {
		return fmt.Errorf("resource pack chunk data had chunk index %v, but expected %v", pk.ChunkIndex, downloadingPack.expectedIndex)
	}
	downloadingPack.expectedIndex++
	downloadingPack.newFrag <- pk.Data
	return nil
}

// handleResourcePackChunkRequest handles a resource pack chunk request, which requests a part of the resource
// pack to be downloaded.
func (conn *Conn) handleResourcePackChunkRequest(pk *packet.ResourcePackChunkRequest) error {
	current := conn.packQueue.currentPack
	if current.UUID() != pk.UUID {
		return fmt.Errorf("resource pack chunk request had unexpected UUID: expected %v, but got %v", current.UUID(), pk.UUID)
	}
	if conn.packQueue.currentOffset != int64(pk.ChunkIndex)*packChunkSize {
		return fmt.Errorf("resource pack chunk request had unexpected chunk index: expected %v, but got %v", conn.packQueue.currentOffset/packChunkSize, pk.ChunkIndex)
	}
	response := &packet.ResourcePackChunkData{
		UUID:       pk.UUID,
		ChunkIndex: pk.ChunkIndex,
		DataOffset: conn.packQueue.currentOffset,
		Data:       make([]byte, packChunkSize),
	}
	conn.packQueue.currentOffset += packChunkSize
	// We read the data directly into the response's data.
	if n, err := current.ReadAt(response.Data, response.DataOffset); err != nil {
		// If we hit an EOF, we don't need to return an error, as we've simply reached the end of the content
		// AKA the last chunk.
		if err != io.EOF {
			return fmt.Errorf("error reading resource pack chunk: %v", err)
		}
		response.Data = response.Data[:n]

		defer func() {
			if !conn.packQueue.AllDownloaded() {
				_ = conn.nextResourcePackDownload()
			} else {
				conn.expect(packet.IDResourcePackClientResponse)
			}
		}()
	}
	if err := conn.WritePacket(response); err != nil {
		return fmt.Errorf("error writing resource pack chunk data packet: %v", err)
	}

	return nil
}

// handlePlayStatus handles an incoming PlayStatus packet. It reacts differently depending on the status
// found in the packet.
func (conn *Conn) handlePlayStatus(pk *packet.PlayStatus) error {
	switch pk.Status {
	case packet.PlayStatusLoginSuccess:
		// The next packet we expect is the ResourcePacksInfo packet.
		conn.expect(packet.IDResourcePacksInfo)
	case packet.PlayStatusLoginFailedClient:
		_ = conn.Close()
		return fmt.Errorf("client outdated")
	case packet.PlayStatusLoginFailedServer:
		_ = conn.Close()
		return fmt.Errorf("server outdated")
	case packet.PlayStatusPlayerSpawn:
		// TODO
	case packet.PlayStatusLoginFailedInvalidTenant:
		_ = conn.Close()
		return fmt.Errorf("invalid edu edition game owner")
	case packet.PlayStatusLoginFailedVanillaEdu:
		_ = conn.Close()
		return fmt.Errorf("cannot join an edu edition game on vanilla")
	case packet.PlayStatusLoginFailedEduVanilla:
		_ = conn.Close()
		return fmt.Errorf("cannot join a vanilla game on edu edition")
	case packet.PlayStatusLoginFailedServerFull:
		_ = conn.Close()
		return fmt.Errorf("server full")
	default:
		return fmt.Errorf("unknown play status in PlayStatus packet %v", pk.Status)
	}
	return nil
}

// enableEncryption enables encryption on the server side over the connection. It sends an unencrypted
// handshake packet to the client and enables encryption after that.
func (conn *Conn) enableEncryption(clientPublicKey *ecdsa.PublicKey) error {
	pubKey, err := jwt.MarshalPublicKey(&conn.privateKey.PublicKey)
	if err != nil {
		return fmt.Errorf("error marshaling public key: %v", err)
	}
	header := jwt.Header{
		Algorithm: "ES384",
		X5U:       pubKey,
	}
	payload := map[string]interface{}{
		"salt": base64.StdEncoding.EncodeToString(conn.salt),
	}

	// We produce an encoded JWT using the header and payload above, then we send the JWT in a ServerToClient-
	// Handshake packet so that the client can initialise encryption.
	serverJWT, err := jwt.New(header, payload, conn.privateKey)
	if err != nil {
		return fmt.Errorf("error creating encoded JWT: %v", err)
	}
	if err := conn.WritePacket(&packet.ServerToClientHandshake{JWT: serverJWT}); err != nil {
		return fmt.Errorf("error sending ServerToClientHandshake packet: %v", err)
	}
	// Flush immediately as we'll enable encryption after this.
	_ = conn.Flush()

	// We first compute the shared secret.
	x, _ := clientPublicKey.Curve.ScalarMult(clientPublicKey.X, clientPublicKey.Y, conn.privateKey.D.Bytes())
	sharedSecret := x.Bytes()
	keyBytes := sha256.Sum256(append(conn.salt, sharedSecret...))

	// Finally we enable encryption for the encoder and decoder using the secret key bytes we produced.
	conn.encoder.EnableEncryption(keyBytes)
	conn.decoder.EnableEncryption(keyBytes)

	return nil
}

// expect sets the packet IDs that are next expected to arrive.
func (conn *Conn) expect(packetIDs ...uint32) {
	conn.expectedIDs = packetIDs
}
