package mint

import (
	"bytes"
	"crypto"
	"crypto/x509"
	"fmt"
	"io"
	"net"
	"sync"
	"time"
)

// Config is the struct used to pass configuration settings to a TLS client or
// server instance.  The settings for client and server are pretty different,
// but we just throw them all in here.
type Config struct {
	// TODO
	ServerName string
}

func (c Config) validForServer() bool {
	// TODO
	return true
}

func (c Config) validForClient() bool {
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

func newConn(conn net.Conn, config *Config, isClient bool) *Conn {
	c := &Conn{conn: conn, config: config, isClient: isClient}
	c.in = newRecordLayer(c.conn)
	c.out = newRecordLayer(c.conn)
	return c
}

func (c *Conn) extendBuffer(n int) error {
	// XXX: crypto/tls bounds the number of empty records that can be read.  Should we?
	// if there's no more data left, stop reading
	if len(c.in.nextData) == 0 && len(c.readBuffer) > 0 {
		return nil
	}

	for len(c.readBuffer) <= n {
		pt, err := c.in.ReadRecord()

		if pt == nil {
			return err
		}

		switch pt.contentType {
		case recordTypeHandshake:
			// TODO: Handle post-handshake handshake messages
		case recordTypeAlert:
			logf(logTypeIO, "extended buffer (for alert): [%d] %x", len(c.readBuffer), c.readBuffer)
			if len(pt.fragment) != 2 {
				c.sendAlert(alertUnexpectedMessage)
				return io.EOF
			}
			if alert(pt.fragment[1]) == alertCloseNotify {
				return io.EOF
			}

			switch pt.fragment[0] {
			case alertLevelWarning:
				// drop on the floor
			case alertLevelError:
				return alert(pt.fragment[1])
			default:
				c.sendAlert(alertUnexpectedMessage)
				return io.EOF
			}

		case recordTypeApplicationData:
			c.readBuffer = append(c.readBuffer, pt.fragment...)
			logf(logTypeIO, "extended buffer: [%d] %x", len(c.readBuffer), c.readBuffer)
		}

		if err != nil {
			return err
		}

		// if there's no more data left, stop reading
		if len(c.in.nextData) == 0 {
			return nil
		}

		// if we're over the limit and the next record is not an alert, exit
		if len(c.readBuffer) == n && recordType(c.in.nextData[0]) != recordTypeAlert {
			return nil
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

	n := len(buffer)
	err := c.extendBuffer(n)
	var read int
	if len(c.readBuffer) < n {
		buffer = buffer[:len(c.readBuffer)]
		copy(buffer, c.readBuffer)
		read = len(c.readBuffer)
		c.readBuffer = c.readBuffer[:0]
	} else {
		logf(logTypeIO, "read buffer larger than than input buffer")
		copy(buffer[:n], c.readBuffer[:n])
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

// sendAlert sends a TLS alert message.
// c.out.Mutex <= L.
func (c *Conn) sendAlert(err alert) error {
	c.handshakeMutex.Lock()
	defer c.handshakeMutex.Unlock()

	tmp := make([]byte, 2)
	switch err {
	case alertNoRenegotiation, alertCloseNotify:
		tmp[0] = alertLevelWarning
	default:
		tmp[0] = alertLevelError
	}
	tmp[1] = byte(err)
	c.out.WriteRecord(&tlsPlaintext{
		contentType: recordTypeAlert,
		fragment:    tmp},
	)

	// closeNotify is a special case in that it isn't an error:
	if err != alertCloseNotify {
		return &net.OpError{Op: "local error", Err: err}
	}
	return nil
}

// Close closes the connection.
func (c *Conn) Close() error {
	// XXX crypto/tls has an interlock with Write here.  Do we need that?

	c.sendAlert(alertCloseNotify)
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

// Handshake causes a TLS handshake on the connection.  The `isClient` member
// determines whether a client or server handshake is performed.  If a
// handshake has already been performed, then its result will be returned.
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
	c.handshakeComplete = (c.handshakeErr == nil)
	return c.handshakeErr
}

func (c *Conn) clientHandshake() error {
	hIn := newHandshakeLayer(c.in)
	hOut := newHandshakeLayer(c.out)

	// XXX Config
	config := struct {
		serverName   string
		authCallback func(chain []*x509.Certificate) error
	}{
		serverName:   "example.com",
		authCallback: func(chain []*x509.Certificate) error { return nil },
	}

	// Construct some extensions
	privateKeys := map[namedGroup][]byte{}
	ks := keyShareExtension{
		roleIsServer: false,
		shares:       make([]keyShare, len(supportedGroups)),
	}
	for i, group := range supportedGroups {
		pub, priv, err := newKeyShare(group)
		if err != nil {
			return err
		}

		ks.shares[i].group = group
		ks.shares[i].keyExchange = pub
		privateKeys[group] = priv
	}
	sni := serverNameExtension(config.serverName)
	sg := supportedGroupsExtension{groups: supportedGroups}
	sa := signatureAlgorithmsExtension{algorithms: signatureAlgorithms}
	dv := draftVersionExtension{version: draftVersionImplemented}

	// Construct and write ClientHello
	ch := &clientHelloBody{
		cipherSuites: supportedCipherSuites,
	}
	for _, ext := range []extensionBody{&sni, &ks, &sg, &sa, &dv} {
		err := ch.extensions.Add(ext)
		if err != nil {
			return err
		}
	}
	chm, err := hOut.WriteMessageBody(ch)
	if err != nil {
		return err
	}
	logf(logTypeHandshake, "Sent ClientHello")

	// Read ServerHello
	sh := new(serverHelloBody)
	shm, err := hIn.ReadMessageBody(sh)
	if err != nil {
		logf(logTypeHandshake, "Error reading ServerHello")
		return err
	}
	logf(logTypeHandshake, "Received ServerHello")

	// Read the key_share extension and do key agreement
	serverKeyShares := keyShareExtension{roleIsServer: true}
	found := sh.extensions.Find(&serverKeyShares)
	if !found {
		logf(logTypeHandshake, "Server key shares extension not found")
		return err
	}
	sks := serverKeyShares.shares[0]
	priv, ok := privateKeys[sks.group]
	if !ok {
		return fmt.Errorf("tls.client: Server sent a private key for a group we didn't send")
	}
	ES, err := keyAgreement(sks.group, sks.keyExchange, priv)
	if err != nil {
		logf(logTypeHandshake, "Error doing key agreement")
		return err
	}
	logf(logTypeHandshake, "Completed key agreement")

	// Init crypto context and rekey
	ctx := cryptoContext{}
	ctx.Init(chm, shm, ES, ES, sh.cipherSuite)
	err = c.in.Rekey(ctx.suite, ctx.handshakeKeys.serverWriteKey, ctx.handshakeKeys.serverWriteIV)
	if err != nil {
		logf(logTypeHandshake, "Unable to rekey inbound")
		return err
	}
	err = c.out.Rekey(ctx.suite, ctx.handshakeKeys.clientWriteKey, ctx.handshakeKeys.clientWriteIV)
	if err != nil {
		logf(logTypeHandshake, "Unable to rekey outbound")
		return err
	}
	logf(logTypeHandshake, "Completed rekey")

	// Read to Finished
	transcript := []*handshakeMessage{}
	var cert *certificateBody
	var certVerify *certificateVerifyBody
	var finishedMessage *handshakeMessage
	for {
		hm, err := hIn.ReadMessage()
		if err != nil {
			logf(logTypeHandshake, "Error reading message: %v", err)
			return err
		}
		logf(logTypeHandshake, "Read message with type: %v", hm.msgType)

		if hm.msgType == handshakeTypeFinished {
			finishedMessage = hm
			break
		} else {
			if hm.msgType == handshakeTypeCertificate {
				cert = new(certificateBody)
				_, err = cert.Unmarshal(hm.body)
			} else if hm.msgType == handshakeTypeCertificateVerify {
				certVerify = new(certificateVerifyBody)
				_, err = certVerify.Unmarshal(hm.body)
			}
			transcript = append(transcript, hm)
		}

		if err != nil {
			logf(logTypeHandshake, "Error processing handshake message: %v", err)
			return err
		}
	}
	logf(logTypeHandshake, "Done reading server's first flight")

	// Verify the server's certificate if required
	if config.authCallback != nil {
		if cert == nil || certVerify == nil {
			return fmt.Errorf("tls.client: No server auth data provided")
		}

		transcriptForCertVerify := append([]*handshakeMessage{chm, shm}, transcript[:len(transcript)-1]...)
		logf(logTypeHandshake, "Transcript for certVerify")
		for _, hm := range transcriptForCertVerify {
			logf(logTypeHandshake, "  [%d] %x", hm.msgType, hm.body)
		}
		logf(logTypeHandshake, "===")

		serverPublicKey := cert.certificateList[0].PublicKey
		if err = certVerify.Verify(serverPublicKey, transcriptForCertVerify); err != nil {
			return err
		}

		if err = config.authCallback(cert.certificateList); err != nil {
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

	_, err = hOut.WriteMessageBody(ctx.clientFinished)
	if err != nil {
		return err
	}

	// Rekey to application keys
	err = c.in.Rekey(ctx.suite, ctx.applicationKeys.serverWriteKey, ctx.applicationKeys.serverWriteIV)
	if err != nil {
		return err
	}
	err = c.out.Rekey(ctx.suite, ctx.applicationKeys.clientWriteKey, ctx.applicationKeys.clientWriteIV)
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
	config := struct {
		supportedGroup       map[namedGroup]bool
		supportedCiphersuite map[cipherSuite]bool
		privateKey           crypto.Signer
		certicate            *x509.Certificate
	}{
		supportedGroup: map[namedGroup]bool{
			namedGroupP256: true,
			namedGroupP384: true,
			namedGroupP521: true,
		},
		supportedCiphersuite: map[cipherSuite]bool{
			TLS_ECDHE_ECDSA_WITH_AES_128_GCM_SHA256: true,
			TLS_ECDHE_RSA_WITH_AES_128_GCM_SHA256:   true,
		},
	}
	config.privateKey, _ = newSigningKey(signatureAlgorithmRSA)
	config.certicate, _ = newSelfSigned("example.com",
		signatureAndHashAlgorithm{hashAlgorithmSHA256, signatureAlgorithmRSA}, config.privateKey)

	// Read ClientHello and extract extensions
	ch := new(clientHelloBody)
	chm, err := hIn.ReadMessageBody(ch)
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
		return fmt.Errorf("tls.server: Missing extension in ClientHello (%v %v %v %v)",
			gotServerName, gotSupportedGroups, gotSignatureAlgorithms, gotKeyShares)
	}

	// Find key_share extension and do key agreement
	var serverKeyShare *keyShareExtension
	var ES []byte
	for _, share := range clientKeyShares.shares {
		if config.supportedGroup[share.group] {
			pub, priv, err := newKeyShare(share.group)
			if err != nil {
				return err
			}

			ES, err = keyAgreement(share.group, share.keyExchange, priv)
			serverKeyShare = &keyShareExtension{
				roleIsServer: true,
				shares:       []keyShare{keyShare{group: share.group, keyExchange: pub}},
			}
			if err != nil {
				return err
			}
			break
		}
	}
	if serverKeyShare == nil {
		return fmt.Errorf("tls.server: Did not find a matching key share")
	}
	if len(ES) == 0 {
		return fmt.Errorf("tls.server: Key agreement failed")
	}

	// Pick a ciphersuite
	var chosenSuite cipherSuite
	foundCipherSuite := false
	for _, suite := range ch.cipherSuites {
		if config.supportedCiphersuite[suite] {
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
	shm, err := hOut.WriteMessageBody(sh)
	if err != nil {
		return err
	}

	// Init context and rekey to handshake keys
	ctx := cryptoContext{}
	ctx.Init(chm, shm, ES, ES, chosenSuite)
	err = c.in.Rekey(ctx.suite, ctx.handshakeKeys.clientWriteKey, ctx.handshakeKeys.clientWriteIV)
	if err != nil {
		return err
	}
	err = c.out.Rekey(ctx.suite, ctx.handshakeKeys.serverWriteKey, ctx.handshakeKeys.serverWriteIV)
	if err != nil {
		return err
	}

	// Send an EncryptedExtensions message (even if it's empty)
	ee := &encryptedExtensionsBody{}
	eem, err := hOut.WriteMessageBody(ee)
	if err != nil {
		return err
	}

	// Create and send Certificate, CertificateVerify
	// TODO Certificate selection based on ClientHello
	certificate := &certificateBody{
		certificateList: []*x509.Certificate{config.certicate},
	}
	certm, err := hOut.WriteMessageBody(certificate)
	if err != nil {
		return err
	}

	certificateVerify := &certificateVerifyBody{
		alg: signatureAndHashAlgorithm{hashAlgorithmSHA256, signatureAlgorithmRSA},
	}
	err = certificateVerify.Sign(config.privateKey, []*handshakeMessage{chm, shm, eem, certm})
	if err != nil {
		return err
	}
	certvm, err := hOut.WriteMessageBody(certificateVerify)
	if err != nil {
		return err
	}

	// Update the crypto context
	ctx.Update([]*handshakeMessage{eem, certm, certvm})

	// Create and write server Finished
	_, err = hOut.WriteMessageBody(ctx.serverFinished)
	if err != nil {
		return err
	}

	// Read and verify client Finished
	cfin := new(finishedBody)
	cfin.verifyDataLen = ctx.clientFinished.verifyDataLen
	_, err = hIn.ReadMessageBody(cfin)
	if err != nil {
		return err
	}
	if !bytes.Equal(cfin.verifyData, ctx.clientFinished.verifyData) {
		return fmt.Errorf("tls.client: Client's Finished failed to verify")
	}

	// Rekey to application keys
	err = c.in.Rekey(ctx.suite, ctx.applicationKeys.clientWriteKey, ctx.applicationKeys.clientWriteIV)
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
