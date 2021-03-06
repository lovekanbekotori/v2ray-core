package encoding

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/md5"
	"hash/fnv"
	"io"
	"sync"
	"time"

	"v2ray.com/core/common/dice"

	"golang.org/x/crypto/chacha20poly1305"
	"v2ray.com/core/common"
	"v2ray.com/core/common/bitmask"
	"v2ray.com/core/common/buf"
	"v2ray.com/core/common/crypto"
	"v2ray.com/core/common/net"
	"v2ray.com/core/common/protocol"
	"v2ray.com/core/common/serial"
	"v2ray.com/core/common/signal"
	"v2ray.com/core/proxy/vmess"
)

type sessionId struct {
	user  [16]byte
	key   [16]byte
	nonce [16]byte
}

type SessionHistory struct {
	sync.RWMutex
	cache map[sessionId]time.Time
	task  *signal.PeriodicTask
}

func NewSessionHistory() *SessionHistory {
	h := &SessionHistory{
		cache: make(map[sessionId]time.Time, 128),
	}
	h.task = &signal.PeriodicTask{
		Interval: time.Second * 30,
		Execute: func() error {
			h.removeExpiredEntries()
			return nil
		},
	}
	common.Must(h.task.Start())
	return h
}

// Close implements common.Closable.
func (h *SessionHistory) Close() error {
	return h.task.Close()
}

func (h *SessionHistory) addIfNotExits(session sessionId) bool {
	h.Lock()
	defer h.Unlock()

	if expire, found := h.cache[session]; found && expire.After(time.Now()) {
		return false
	}

	h.cache[session] = time.Now().Add(time.Minute * 3)
	return true
}

func (h *SessionHistory) removeExpiredEntries() {
	now := time.Now()

	h.Lock()
	defer h.Unlock()

	for session, expire := range h.cache {
		if expire.Before(now) {
			delete(h.cache, session)
		}
	}
}

type ServerSession struct {
	userValidator   protocol.UserValidator
	sessionHistory  *SessionHistory
	requestBodyKey  []byte
	requestBodyIV   []byte
	responseBodyKey []byte
	responseBodyIV  []byte
	responseHeader  byte
	responseWriter  io.Writer
}

// NewServerSession creates a new ServerSession, using the given UserValidator.
// The ServerSession instance doesn't take ownership of the validator.
func NewServerSession(validator protocol.UserValidator, sessionHistory *SessionHistory) *ServerSession {
	return &ServerSession{
		userValidator:  validator,
		sessionHistory: sessionHistory,
	}
}

func parseSecurityType(b byte) protocol.SecurityType {
	if _, f := protocol.SecurityType_name[int32(b)]; f {
		return protocol.SecurityType(b)
	}
	return protocol.SecurityType_UNKNOWN
}

func (s *ServerSession) DecodeRequestHeader(reader io.Reader) (*protocol.RequestHeader, error) {
	buffer := buf.New()
	defer buffer.Release()

	if err := buffer.AppendSupplier(buf.ReadFullFrom(reader, protocol.IDBytesLen)); err != nil {
		return nil, newError("failed to read request header").Base(err)
	}

	user, timestamp, valid := s.userValidator.Get(buffer.Bytes())
	if !valid {
		return nil, newError("invalid user")
	}

	timestampHash := md5.New()
	common.Must2(timestampHash.Write(hashTimestamp(timestamp)))
	iv := timestampHash.Sum(nil)
	account, err := user.GetTypedAccount()
	if err != nil {
		return nil, newError("failed to get user account").Base(err)
	}
	vmessAccount := account.(*vmess.InternalAccount)

	aesStream := crypto.NewAesDecryptionStream(vmessAccount.ID.CmdKey(), iv)
	decryptor := crypto.NewCryptionReader(aesStream, reader)

	if err := buffer.Reset(buf.ReadFullFrom(decryptor, 38)); err != nil {
		return nil, newError("failed to read request header").Base(err)
	}

	request := &protocol.RequestHeader{
		User:    user,
		Version: buffer.Byte(0),
	}

	s.requestBodyIV = append([]byte(nil), buffer.BytesRange(1, 17)...)   // 16 bytes
	s.requestBodyKey = append([]byte(nil), buffer.BytesRange(17, 33)...) // 16 bytes
	var sid sessionId
	copy(sid.user[:], vmessAccount.ID.Bytes())
	copy(sid.key[:], s.requestBodyKey)
	copy(sid.nonce[:], s.requestBodyIV)
	if !s.sessionHistory.addIfNotExits(sid) {
		return nil, newError("duplicated session id, possibly under replay attack")
	}

	s.responseHeader = buffer.Byte(33)             // 1 byte
	request.Option = bitmask.Byte(buffer.Byte(34)) // 1 byte
	padingLen := int(buffer.Byte(35) >> 4)
	request.Security = parseSecurityType(buffer.Byte(35) & 0x0F)
	// 1 bytes reserved
	request.Command = protocol.RequestCommand(buffer.Byte(37))

	var invalidRequestErr error
	defer func() {
		if invalidRequestErr != nil {
			randomLen := dice.Roll(64) + 1
			// Read random number of bytes for prevent detection.
			buffer.AppendSupplier(buf.ReadFullFrom(decryptor, randomLen))
		}
	}()

	if request.Security == protocol.SecurityType_UNKNOWN || request.Security == protocol.SecurityType_AUTO {
		invalidRequestErr = newError("unknown security type")
		return nil, invalidRequestErr
	}

	switch request.Command {
	case protocol.RequestCommandMux:
		request.Address = net.DomainAddress("v1.mux.cool")
		request.Port = 0
	case protocol.RequestCommandTCP, protocol.RequestCommandUDP:
		if addr, port, err := addrParser.ReadAddressPort(buffer, decryptor); err == nil {
			request.Address = addr
			request.Port = port
		} else {
			invalidRequestErr = newError("invalid address").Base(err)
			return nil, invalidRequestErr
		}
	default:
		invalidRequestErr = newError("invalid request command: ", request.Command)
		return nil, invalidRequestErr
	}

	if padingLen > 0 {
		if err := buffer.AppendSupplier(buf.ReadFullFrom(decryptor, padingLen)); err != nil {
			return nil, newError("failed to read padding").Base(err)
		}
	}

	if err := buffer.AppendSupplier(buf.ReadFullFrom(decryptor, 4)); err != nil {
		return nil, newError("failed to read checksum").Base(err)
	}

	fnv1a := fnv.New32a()
	common.Must2(fnv1a.Write(buffer.BytesTo(-4)))
	actualHash := fnv1a.Sum32()
	expectedHash := serial.BytesToUint32(buffer.BytesFrom(-4))

	if actualHash != expectedHash {
		return nil, newError("invalid auth")
	}

	if request.Address == nil {
		return nil, newError("invalid remote address")
	}

	return request, nil
}

func (s *ServerSession) DecodeRequestBody(request *protocol.RequestHeader, reader io.Reader) buf.Reader {
	var sizeParser crypto.ChunkSizeDecoder = crypto.PlainChunkSizeParser{}
	if request.Option.Has(protocol.RequestOptionChunkMasking) {
		sizeParser = NewShakeSizeParser(s.requestBodyIV)
	}
	switch request.Security {
	case protocol.SecurityType_NONE:
		if request.Option.Has(protocol.RequestOptionChunkStream) {
			if request.Command.TransferType() == protocol.TransferTypeStream {
				return crypto.NewChunkStreamReader(sizeParser, reader)
			}

			auth := &crypto.AEADAuthenticator{
				AEAD:                    new(NoOpAuthenticator),
				NonceGenerator:          crypto.NoOpBytesGenerator{},
				AdditionalDataGenerator: crypto.NoOpBytesGenerator{},
			}
			return crypto.NewAuthenticationReader(auth, sizeParser, reader, protocol.TransferTypePacket)
		}

		return buf.NewReader(reader)
	case protocol.SecurityType_LEGACY:
		aesStream := crypto.NewAesDecryptionStream(s.requestBodyKey, s.requestBodyIV)
		cryptionReader := crypto.NewCryptionReader(aesStream, reader)
		if request.Option.Has(protocol.RequestOptionChunkStream) {
			auth := &crypto.AEADAuthenticator{
				AEAD:                    new(FnvAuthenticator),
				NonceGenerator:          crypto.NoOpBytesGenerator{},
				AdditionalDataGenerator: crypto.NoOpBytesGenerator{},
			}
			return crypto.NewAuthenticationReader(auth, sizeParser, cryptionReader, request.Command.TransferType())
		}

		return buf.NewReader(cryptionReader)
	case protocol.SecurityType_AES128_GCM:
		block, _ := aes.NewCipher(s.requestBodyKey)
		aead, _ := cipher.NewGCM(block)

		auth := &crypto.AEADAuthenticator{
			AEAD: aead,
			NonceGenerator: &ChunkNonceGenerator{
				Nonce: append([]byte(nil), s.requestBodyIV...),
				Size:  aead.NonceSize(),
			},
			AdditionalDataGenerator: crypto.NoOpBytesGenerator{},
		}
		return crypto.NewAuthenticationReader(auth, sizeParser, reader, request.Command.TransferType())
	case protocol.SecurityType_CHACHA20_POLY1305:
		aead, _ := chacha20poly1305.New(GenerateChacha20Poly1305Key(s.requestBodyKey))

		auth := &crypto.AEADAuthenticator{
			AEAD: aead,
			NonceGenerator: &ChunkNonceGenerator{
				Nonce: append([]byte(nil), s.requestBodyIV...),
				Size:  aead.NonceSize(),
			},
			AdditionalDataGenerator: crypto.NoOpBytesGenerator{},
		}
		return crypto.NewAuthenticationReader(auth, sizeParser, reader, request.Command.TransferType())
	default:
		panic("Unknown security type.")
	}
}

func (s *ServerSession) EncodeResponseHeader(header *protocol.ResponseHeader, writer io.Writer) {
	responseBodyKey := md5.Sum(s.requestBodyKey)
	responseBodyIV := md5.Sum(s.requestBodyIV)
	s.responseBodyKey = responseBodyKey[:]
	s.responseBodyIV = responseBodyIV[:]

	aesStream := crypto.NewAesEncryptionStream(s.responseBodyKey, s.responseBodyIV)
	encryptionWriter := crypto.NewCryptionWriter(aesStream, writer)
	s.responseWriter = encryptionWriter

	common.Must2(encryptionWriter.Write([]byte{s.responseHeader, byte(header.Option)}))
	err := MarshalCommand(header.Command, encryptionWriter)
	if err != nil {
		common.Must2(encryptionWriter.Write([]byte{0x00, 0x00}))
	}
}

func (s *ServerSession) EncodeResponseBody(request *protocol.RequestHeader, writer io.Writer) buf.Writer {
	var sizeParser crypto.ChunkSizeEncoder = crypto.PlainChunkSizeParser{}
	if request.Option.Has(protocol.RequestOptionChunkMasking) {
		sizeParser = NewShakeSizeParser(s.responseBodyIV)
	}
	switch request.Security {
	case protocol.SecurityType_NONE:
		if request.Option.Has(protocol.RequestOptionChunkStream) {
			if request.Command.TransferType() == protocol.TransferTypeStream {
				return crypto.NewChunkStreamWriter(sizeParser, writer)
			}

			auth := &crypto.AEADAuthenticator{
				AEAD:                    new(NoOpAuthenticator),
				NonceGenerator:          &crypto.NoOpBytesGenerator{},
				AdditionalDataGenerator: crypto.NoOpBytesGenerator{},
			}
			return crypto.NewAuthenticationWriter(auth, sizeParser, writer, protocol.TransferTypePacket)
		}

		return buf.NewWriter(writer)
	case protocol.SecurityType_LEGACY:
		if request.Option.Has(protocol.RequestOptionChunkStream) {
			auth := &crypto.AEADAuthenticator{
				AEAD:                    new(FnvAuthenticator),
				NonceGenerator:          crypto.NoOpBytesGenerator{},
				AdditionalDataGenerator: crypto.NoOpBytesGenerator{},
			}
			return crypto.NewAuthenticationWriter(auth, sizeParser, s.responseWriter, request.Command.TransferType())
		}

		return buf.NewWriter(s.responseWriter)
	case protocol.SecurityType_AES128_GCM:
		block, _ := aes.NewCipher(s.responseBodyKey)
		aead, _ := cipher.NewGCM(block)

		auth := &crypto.AEADAuthenticator{
			AEAD: aead,
			NonceGenerator: &ChunkNonceGenerator{
				Nonce: append([]byte(nil), s.responseBodyIV...),
				Size:  aead.NonceSize(),
			},
			AdditionalDataGenerator: crypto.NoOpBytesGenerator{},
		}
		return crypto.NewAuthenticationWriter(auth, sizeParser, writer, request.Command.TransferType())
	case protocol.SecurityType_CHACHA20_POLY1305:
		aead, _ := chacha20poly1305.New(GenerateChacha20Poly1305Key(s.responseBodyKey))

		auth := &crypto.AEADAuthenticator{
			AEAD: aead,
			NonceGenerator: &ChunkNonceGenerator{
				Nonce: append([]byte(nil), s.responseBodyIV...),
				Size:  aead.NonceSize(),
			},
			AdditionalDataGenerator: crypto.NoOpBytesGenerator{},
		}
		return crypto.NewAuthenticationWriter(auth, sizeParser, writer, request.Command.TransferType())
	default:
		panic("Unknown security type.")
	}
}
