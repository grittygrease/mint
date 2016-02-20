package mint

import (
	"bytes"
	"crypto/x509"
	"fmt"
	"net"
	"sync"
	"time"
)

type Config struct {
	// TODO
	ServerName string
}

func (c Config) ValidForServer() bool {
	// TODO
	return true
}

func (c Config) ValidForClient() bool {
	// TODO
	return true
}

func defaultConfig() *Config {
	// TODO
	return &Config{}
}

var (
	supportedCipherSuites = []cipherSuite{
		TLS_ECDHE_ECDSA_WITH_AES_128_GCM_SHA256,
		TLS_ECDHE_RSA_WITH_AES_128_GCM_SHA256,
		TLS_ECDHE_ECDSA_WITH_AES_256_GCM_SHA384,
		TLS_ECDHE_RSA_WITH_AES_256_GCM_SHA384,
	}

	supportedGroups = []namedGroup{
		namedGroupP256,
		namedGroupP384,
		namedGroupP521,
	}

	signatureAlgorithms = []signatureAndHashAlgorithm{
		signatureAndHashAlgorithm{hashAlgorithmSHA256, signatureAlgorithmRSA},
		signatureAndHashAlgorithm{hashAlgorithmSHA256, signatureAlgorithmECDSA},
		signatureAndHashAlgorithm{hashAlgorithmSHA384, signatureAlgorithmRSA},
		signatureAndHashAlgorithm{hashAlgorithmSHA384, signatureAlgorithmECDSA},
		signatureAndHashAlgorithm{hashAlgorithmSHA512, signatureAlgorithmRSA},
		signatureAndHashAlgorithm{hashAlgorithmSHA512, signatureAlgorithmECDSA},
	}
)

// Conn implements the net.Conn interface, as with "crypto/tls"
// * Read, Write, and Close are provided locally
// * LocalAddr, RemoteAddr, and Set*Deadline are forwarded to the inner Conn
type Conn struct {
	config   *Config
	conn     net.Conn
	isClient bool

	handshakeMutex    sync.Mutex
	handshakeErr      error
	handshakeComplete bool

	readBuffer        []byte
	in, out           *recordLayer
	inMutex, outMutex sync.Mutex
	context           cryptoContext
}

func (c *Conn) extendBuffer(n int) error {
	// XXX: crypto/tls bounds the number of empty records that can be read.  Should we?
	for len(c.readBuffer) < n {
		pt, err := c.in.ReadRecord()
		if err != nil {
			return err
		}

		switch pt.contentType {
		case recordTypeHandshake:
			// TODO: Handle post-handshake handshake messages
		case recordTypeAlert:
			// TODO: Handle alerts
		case recordTypeApplicationData:
			c.readBuffer = append(c.readBuffer, pt.fragment...)
		}
	}
	return nil
}

// Read application data until the buffer is full.  Handshake and alert records
// are consumed by the Conn object directly.
func (c *Conn) Read(buffer []byte) (int, error) {
	if err := c.Handshake(); err != nil {
		return 0, err
	}

	// Lock the input channel
	c.in.Lock()
	defer c.in.Unlock()

	n := cap(buffer)
	err := c.extendBuffer(n)
	var read int
	if len(c.readBuffer) < n {
		copy(buffer[:0], c.readBuffer)
		c.readBuffer = c.readBuffer[:0]
		read = len(c.readBuffer)
	} else {
		copy(buffer[:0], c.readBuffer[:n])
		c.readBuffer = c.readBuffer[n:]
		read = n
	}

	return read, err
}

// Write application data
func (c *Conn) Write(buffer []byte) (int, error) {
	// XXX crypto/tls has an interlock with Close here.  Do we need that?
	if err := c.Handshake(); err != nil {
		return 0, err
	}

	// Lock the output channel
	c.out.Lock()
	defer c.out.Unlock()

	// Send full-size fragments
	var start int
	sent := 0
	for start = 0; len(buffer)-start >= maxFragmentLen; start += maxFragmentLen {
		err := c.out.WriteRecord(&tlsPlaintext{
			contentType: recordTypeApplicationData,
			fragment:    buffer[start : start+maxFragmentLen],
		})

		if err != nil {
			return sent, err
		}
		sent += maxFragmentLen
	}

	// Send a final partial fragment if necessary
	if start < len(buffer) {
		err := c.out.WriteRecord(&tlsPlaintext{
			contentType: recordTypeApplicationData,
			fragment:    buffer[start:],
		})

		if err != nil {
			return sent, err
		}
		sent += len(buffer[start:])
	}
	return sent, nil
}

// Close closes the connection.
func (c *Conn) Close() error {
	// XXX crypto/tls has an interlock with Write here.  Do we need that?

	c.handshakeMutex.Lock()
	defer c.handshakeMutex.Unlock()

	// TODO Send closeNotify alert
	return c.conn.Close()
}

// LocalAddr returns the local network address.
func (c *Conn) LocalAddr() net.Addr {
	return c.conn.LocalAddr()
}

// RemoteAddr returns the remote network address.
func (c *Conn) RemoteAddr() net.Addr {
	return c.conn.RemoteAddr()
}

// SetDeadline sets the read and write deadlines associated with the connection.
// A zero value for t means Read and Write will not time out.
// After a Write has timed out, the TLS state is corrupt and all future writes will return the same error.
func (c *Conn) SetDeadline(t time.Time) error {
	return c.conn.SetDeadline(t)
}

// SetReadDeadline sets the read deadline on the underlying connection.
// A zero value for t means Read will not time out.
func (c *Conn) SetReadDeadline(t time.Time) error {
	return c.conn.SetReadDeadline(t)
}

// SetWriteDeadline sets the write deadline on the underlying connection.
// A zero value for t means Write will not time out.
// After a Write has timed out, the TLS state is corrupt and all future writes will return the same error.
func (c *Conn) SetWriteDeadline(t time.Time) error {
	return c.conn.SetWriteDeadline(t)
}

func (c *Conn) Handshake() error {
	// TODO Lock handshakeMutex
	if err := c.handshakeErr; err != nil {
		return err
	}
	if c.handshakeComplete {
		return nil
	}

	if c.isClient {
		c.handshakeErr = c.clientHandshake()
	} else {
		c.handshakeErr = c.serverHandshake()
	}
	return c.handshakeErr
}

func (c *Conn) clientHandshake() error {
	hIn := newHandshakeLayer(c.in)
	hOut := newHandshakeLayer(c.out)

	// XXX Config
	config_serverName := "example.com"
	config_cipherSuites := []cipherSuite{
		TLS_ECDHE_ECDSA_WITH_AES_128_GCM_SHA256,
		TLS_ECDHE_RSA_WITH_AES_128_GCM_SHA256,
	}
	config_keyShareGroups := []namedGroup{namedGroupP256, namedGroupP384, namedGroupP521}
	config_signatureAlgorithms := []signatureAndHashAlgorithm{
		signatureAndHashAlgorithm{hash: hashAlgorithmSHA256, signature: signatureAlgorithmRSA},
		signatureAndHashAlgorithm{hash: hashAlgorithmSHA384, signature: signatureAlgorithmECDSA},
	}
	config_authenticationCallback := func(chain []*x509.Certificate) error { return nil }

	// Construct some extensions
	privateKeys := map[namedGroup][]byte{}
	ks := keyShareExtension{
		roleIsServer: false,
		shares:       make([]keyShare, len(config_keyShareGroups)),
	}
	for i, group := range config_keyShareGroups {
		pub, priv, err := newKeyShare(group)
		if err != nil {
			return err
		}

		ks.shares[i].group = group
		ks.shares[i].keyExchange = pub
		privateKeys[group] = priv
	}
	sni := serverNameExtension(config_serverName)
	sg := supportedGroupsExtension{groups: config_keyShareGroups}
	sa := signatureAlgorithmsExtension{algorithms: config_signatureAlgorithms}

	// Construct and write ClientHello
	ch := &clientHelloBody{
		cipherSuites: config_cipherSuites,
	}
	ch.extensions.Add(&sni)
	ch.extensions.Add(&ks)
	ch.extensions.Add(&sg)
	ch.extensions.Add(&sa)
	err := hOut.WriteMessageBody(ch)
	if err != nil {
		return err
	}

	// Read ServerHello
	sh := new(serverHelloBody)
	err = hIn.ReadMessageBody(sh)
	if err != nil {
		return err
	}

	// Read the key_share extension and do key agreement
	serverKeyShares := keyShareExtension{roleIsServer: true}
	found := sh.extensions.Find(&serverKeyShares)
	if !found {
		return err
	}
	sks := serverKeyShares.shares[0]
	priv, ok := privateKeys[sks.group]
	if !ok {
		fmt.Errorf("tls.client: Server sent a private key for a group we didn't send")
	}
	ES, err := keyAgreement(sks.group, sks.keyExchange, priv)
	if err != nil {
		panic(err)
	}

	// Init crypto context and rekey
	ctx := cryptoContext{}
	ctx.Init(ch, sh, ES, ES, sh.cipherSuite)
	err = c.in.Rekey(ctx.suite, ctx.handshakeKeys.serverWriteKey, ctx.handshakeKeys.serverWriteIV)
	if err != nil {
		return err
	}
	err = c.out.Rekey(ctx.suite, ctx.handshakeKeys.serverWriteKey, ctx.handshakeKeys.serverWriteIV)
	if err != nil {
		return err
	}

	// Read to Finished
	transcript := []handshakeMessageBody{}
	var finishedMessage *handshakeMessage
	for {
		hm, err := hIn.ReadMessage()
		if err != nil {
			return err
		}
		if hm.msgType == handshakeTypeFinished {
			finishedMessage = hm
			break
		}

		body, err := hm.toBody()
		if err != nil {
			return err
		}
		transcript = append(transcript, body)
	}

	// Verify the server's certificate if required
	if config_authenticationCallback != nil {
		transcriptLen := len(transcript)
		if transcriptLen < 2 {
			return fmt.Errorf("tls.client: No authentication data provided (%d)")
		}

		cert, ok := transcript[transcriptLen-2].(*certificateBody)
		if !ok {
			return fmt.Errorf("tls.client: Certificate message not found")
		}

		certVerify, ok := transcript[transcriptLen-1].(*certificateVerifyBody)
		if !ok {
			return fmt.Errorf("tls.client: CertificateVerify message not found")
		}

		// TODO Verify signature over handshake context
		serverPublicKey := cert.certificateList[0].PublicKey
		transcriptForCertVerify := []handshakeMessageBody{ch, sh}
		transcriptForCertVerify = append(transcriptForCertVerify, transcript[:transcriptLen-2]...)
		if err = certVerify.Verify(serverPublicKey, transcriptForCertVerify); err != nil {
			return err
		}

		if err = config_authenticationCallback(cert.certificateList); err != nil {
			return err
		}
	}

	// Update the crypto context with all but the Finished
	ctx.Update(transcript)

	// Verify server finished
	sfin := new(finishedBody)
	sfin.verifyDataLen = ctx.serverFinished.verifyDataLen
	_, err = sfin.Unmarshal(finishedMessage.body)
	if err != nil {
		return err
	}
	if !bytes.Equal(sfin.verifyData, ctx.serverFinished.verifyData) {
		return fmt.Errorf("tls.client: Server's Finished failed to verify")
	}

	// Send client Finished
	err = hOut.WriteMessageBody(ctx.clientFinished)
	if err != nil {
		return err
	}

	// Rekey to application keys
	err = c.in.Rekey(ctx.suite, ctx.applicationKeys.serverWriteKey, ctx.applicationKeys.serverWriteIV)
	if err != nil {
		return err
	}
	err = c.out.Rekey(ctx.suite, ctx.applicationKeys.serverWriteKey, ctx.applicationKeys.serverWriteIV)
	if err != nil {
		return err
	}

	c.context = ctx
	return nil
}

func (c *Conn) serverHandshake() error {
	hIn := newHandshakeLayer(c.in)
	hOut := newHandshakeLayer(c.out)

	// Config
	config_supportedGroup := map[namedGroup]bool{
		namedGroupP384: true,
		namedGroupP521: true,
	}
	config_supportedCiphersuite := map[cipherSuite]bool{
		TLS_ECDHE_ECDSA_WITH_AES_128_GCM_SHA256: true,
		TLS_ECDHE_RSA_WITH_AES_128_GCM_SHA256:   true,
	}
	config_privateKey, _ := newSigningKey(signatureAlgorithmRSA)
	config_serverCertificate, _ := newSelfSigned("example.com",
		signatureAndHashAlgorithm{hashAlgorithmSHA256, signatureAlgorithmRSA}, config_privateKey)

	// Read ClientHello and extract extensions
	ch := new(clientHelloBody)
	err := hIn.ReadMessageBody(ch)
	if err != nil {
		return err
	}

	serverName := new(serverNameExtension)
	supportedGroups := new(supportedGroupsExtension)
	signatureAlgorithms := new(signatureAlgorithmsExtension)
	clientKeyShares := &keyShareExtension{roleIsServer: false}

	gotServerName := ch.extensions.Find(serverName)
	gotSupportedGroups := ch.extensions.Find(supportedGroups)
	gotSignatureAlgorithms := ch.extensions.Find(signatureAlgorithms)
	gotKeyShares := ch.extensions.Find(clientKeyShares)
	if !gotServerName || !gotSupportedGroups || !gotSignatureAlgorithms || !gotKeyShares {
		return fmt.Errorf("tls.server: Missing extension in ClientHello")
	}

	// Find key_share extension and do key agreement
	var serverKeyShare *keyShareExtension
	var ES []byte
	for _, share := range clientKeyShares.shares {
		if config_supportedGroup[share.group] {
			pub, priv, err := newKeyShare(share.group)
			if err != nil {
				return err
			}

			ES, err = keyAgreement(share.group, share.keyExchange, priv)
			serverKeyShare = &keyShareExtension{
				roleIsServer: true,
				shares:       []keyShare{keyShare{group: share.group, keyExchange: pub}},
			}
			break
		}
	}
	if serverKeyShare == nil || len(ES) == 0 {
		return fmt.Errorf("tls.server: Key agreement failed")
	}

	// Pick a ciphersuite
	var chosenSuite cipherSuite
	foundCipherSuite := false
	for _, suite := range ch.cipherSuites {
		if config_supportedCiphersuite[suite] {
			chosenSuite = suite
			foundCipherSuite = true
		}
	}
	if !foundCipherSuite {
		return fmt.Errorf("tls.server: No acceptable ciphersuites")
	}

	// Create and write ServerHello
	sh := &serverHelloBody{
		cipherSuite: chosenSuite,
	}
	sh.extensions.Add(serverKeyShare)
	err = hOut.WriteMessageBody(sh)
	if err != nil {
		return err
	}

	// Init context and rekey to handshake keys
	ctx := cryptoContext{}
	ctx.Init(ch, sh, ES, ES, chosenSuite)
	err = c.in.Rekey(ctx.suite, ctx.handshakeKeys.serverWriteKey, ctx.handshakeKeys.serverWriteIV)
	if err != nil {
		return err
	}
	err = c.out.Rekey(ctx.suite, ctx.handshakeKeys.serverWriteKey, ctx.handshakeKeys.serverWriteIV)
	if err != nil {
		return err
	}

	// Create and send Certificate, CertificateVerify
	// TODO Certificate selection based on ClientHello
	certificate := &certificateBody{
		certificateList: []*x509.Certificate{config_serverCertificate},
	}
	certificateVerify := &certificateVerifyBody{
		alg: signatureAndHashAlgorithm{hashAlgorithmSHA256, signatureAlgorithmRSA},
	}
	err = certificateVerify.Sign(config_privateKey, []handshakeMessageBody{ch, sh})
	if err != nil {
		return err
	}
	err = hOut.WriteMessageBody(certificate)
	if err != nil {
		return err
	}
	err = hOut.WriteMessageBody(certificateVerify)
	if err != nil {
		return err
	}

	// Update the crypto context
	ctx.Update([]handshakeMessageBody{certificate, certificateVerify})

	// Create and write server Finished
	err = hOut.WriteMessageBody(ctx.serverFinished)
	if err != nil {
		return err
	}

	// Read and verify client Finished
	cfin := new(finishedBody)
	cfin.verifyDataLen = ctx.clientFinished.verifyDataLen
	err = hIn.ReadMessageBody(cfin)
	if err != nil {
		return err
	}
	if !bytes.Equal(cfin.verifyData, ctx.clientFinished.verifyData) {
		return fmt.Errorf("tls.client: Client's Finished failed to verify")
	}

	// Rekey to application keys
	err = c.in.Rekey(ctx.suite, ctx.applicationKeys.serverWriteKey, ctx.applicationKeys.serverWriteIV)
	if err != nil {
		return err
	}
	err = c.out.Rekey(ctx.suite, ctx.applicationKeys.serverWriteKey, ctx.applicationKeys.serverWriteIV)
	if err != nil {
		return err
	}

	c.context = ctx
	return nil
}
