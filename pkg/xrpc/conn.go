package xrpc

import (
	"context"
	"crypto/ed25519"
	"encoding/json"
	"errors"
	"fmt"
	"sync"

	"github.com/coder/websocket"

	"github.com/satchel/satchel/pkg/securechan"
)

// ReadLimit 是一条消息的大小上限。
const ReadLimit = 4 << 20

var (
	// ErrPlaintext 是该收加密信封的时候收到了文本帧（含去掉加密的消息）。
	ErrPlaintext = errors.New("xrpc：握手之后收到明文帧")
	// ErrNotPlain 是握手阶段收到了二进制帧。
	ErrNotPlain = errors.New("xrpc：握手阶段收到二进制帧")
	// ErrNotSecured 是握手完成之前要发加密消息，或握手完成之后还要发明文。
	ErrNotSecured = errors.New("xrpc：握手状态不对")
	// ErrBadSignature 是主控对握手的签名验不过：节点配置里的主控公钥与真正的主控对不上。
	ErrBadSignature = errors.New("xrpc：主控的握手签名验不过")
)

// Conn 是一条 websocket 连接：握手阶段收发文本帧，Secure 之后只收发加密信封。
// 写可以并发（发送序号与写出的顺序一致）；读只能由一个 goroutine 做。
type Conn struct {
	ws      *websocket.Conn
	writeMu sync.Mutex
	session *securechan.Session
}

// NewConn 包一条 websocket 连接，并设消息大小上限。
func NewConn(ws *websocket.Conn) *Conn {
	ws.SetReadLimit(ReadLimit)
	return &Conn{ws: ws}
}

// WS 返回底下的 websocket 连接（ping、关闭用）。
func (c *Conn) WS() *websocket.Conn { return c.ws }

// Secure 装上握手得到的会话；此后只收发加密信封。
func (c *Conn) Secure(s *securechan.Session) {
	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	c.session = s
}

func (c *Conn) secured() bool {
	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	return c.session != nil
}

func encode(typ string, payload any) ([]byte, error) {
	var raw json.RawMessage
	if payload != nil {
		b, err := json.Marshal(payload)
		if err != nil {
			return nil, err
		}
		raw = b
	}
	return json.Marshal(Message{Type: typ, Payload: raw})
}

// WritePlain 发一条明文消息（文本帧），只在握手阶段用。
func (c *Conn) WritePlain(ctx context.Context, typ string, payload any) error {
	data, err := encode(typ, payload)
	if err != nil {
		return err
	}
	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	if c.session != nil {
		return ErrNotSecured
	}
	return c.ws.Write(ctx, websocket.MessageText, data)
}

// ReadPlain 读一条明文消息，只在握手阶段用；收到二进制帧返回 ErrNotPlain。
func (c *Conn) ReadPlain(ctx context.Context) (Message, error) {
	typ, data, err := c.ws.Read(ctx)
	if err != nil {
		return Message{}, err
	}
	if typ != websocket.MessageText {
		return Message{}, ErrNotPlain
	}
	var m Message
	if err := json.Unmarshal(data, &m); err != nil {
		return Message{}, fmt.Errorf("xrpc：消息不是合法的 JSON：%w", err)
	}
	return m, nil
}

// Write 发一条加密消息（二进制帧）；握手完成之前返回 ErrNotSecured。
func (c *Conn) Write(ctx context.Context, typ string, payload any) error {
	data, err := encode(typ, payload)
	if err != nil {
		return err
	}
	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	if c.session == nil {
		return ErrNotSecured
	}
	env, err := c.session.Encrypt(data)
	if err != nil {
		return err
	}
	return c.ws.Write(ctx, websocket.MessageBinary, env)
}

// Read 读一条加密消息：文本帧返回 ErrPlaintext，解密失败或序号不对返回 securechan 的错误；调用方遇到任何错误都断开。
func (c *Conn) Read(ctx context.Context) (Message, error) {
	if !c.secured() {
		return Message{}, ErrNotSecured
	}
	typ, data, err := c.ws.Read(ctx)
	if err != nil {
		return Message{}, err
	}
	if typ != websocket.MessageBinary {
		return Message{}, ErrPlaintext
	}
	plain, err := c.session.Decrypt(data)
	if err != nil {
		return Message{}, err
	}
	var m Message
	if err := json.Unmarshal(plain, &m); err != nil {
		return Message{}, fmt.Errorf("xrpc：消息不是合法的 JSON：%w", err)
	}
	return m, nil
}

// Close 以给定的状态码关闭连接。
func (c *Conn) Close(code websocket.StatusCode, reason string) error { return c.ws.Close(code, reason) }

// Decode 把消息的 payload 解进 v。
func Decode(m Message, v any) error {
	if err := json.Unmarshal(m.Payload, v); err != nil {
		return fmt.Errorf("xrpc：%s 消息的内容解不开：%w", m.Type, err)
	}
	return nil
}

// ServerHandshake 是主控一侧的握手：第一帧必须是明文的 key_exchange，回 key_exchange_resp（主控临时公钥与签名），
// 派生会话并装上。任何不符合的情况返回错误，调用方断开。
func ServerHandshake(ctx context.Context, c *Conn, identity ed25519.PrivateKey) error {
	m, err := c.ReadPlain(ctx)
	if err != nil {
		return err
	}
	if m.Type != TypeKeyExchange {
		return fmt.Errorf("xrpc：第一帧应当是 %s，收到 %q", TypeKeyExchange, m.Type)
	}
	var req KeyExchange
	if err := Decode(m, &req); err != nil {
		return err
	}
	if len(req.AgentEphemeral) != securechan.KeySize {
		return errors.New("xrpc：节点临时公钥长度不对")
	}
	priv, pub, err := securechan.GenerateEphemeral()
	if err != nil {
		return err
	}
	shared, err := securechan.ComputeSharedSecret(priv, req.AgentEphemeral)
	if err != nil {
		return err
	}
	session, err := securechan.DeriveSession(shared, req.AgentEphemeral, pub, true)
	if err != nil {
		return err
	}
	sig := securechan.SignHandshake(identity, req.AgentEphemeral, pub)
	if err := c.WritePlain(ctx, TypeKeyExchangeResp, KeyExchangeResp{MasterEphemeral: pub, Signature: sig}); err != nil {
		return err
	}
	c.Secure(session)
	return nil
}

// ClientHandshake 是节点一侧的握手：发 key_exchange，收 key_exchange_resp，用主控公钥验签；验不过返回 ErrBadSignature，
// 此时还没有发出任何秘密。主控公钥为空时直接返回错误，不跳过握手。
func ClientHandshake(ctx context.Context, c *Conn, masterPub ed25519.PublicKey) error {
	if len(masterPub) != ed25519.PublicKeySize {
		return errors.New("xrpc：没有主控公钥，不连")
	}
	priv, pub, err := securechan.GenerateEphemeral()
	if err != nil {
		return err
	}
	if err := c.WritePlain(ctx, TypeKeyExchange, KeyExchange{AgentEphemeral: pub}); err != nil {
		return err
	}
	m, err := c.ReadPlain(ctx)
	if err != nil {
		return err
	}
	if m.Type != TypeKeyExchangeResp {
		return fmt.Errorf("xrpc：握手响应应当是 %s，收到 %q", TypeKeyExchangeResp, m.Type)
	}
	var resp KeyExchangeResp
	if err := Decode(m, &resp); err != nil {
		return err
	}
	if !securechan.VerifyHandshake(masterPub, pub, resp.MasterEphemeral, resp.Signature) {
		return ErrBadSignature
	}
	shared, err := securechan.ComputeSharedSecret(priv, resp.MasterEphemeral)
	if err != nil {
		return err
	}
	session, err := securechan.DeriveSession(shared, pub, resp.MasterEphemeral, false)
	if err != nil {
		return err
	}
	c.Secure(session)
	return nil
}
