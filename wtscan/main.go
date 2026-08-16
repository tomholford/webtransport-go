// Command wtscan is a single-page WebTransport debug/inspect tool.
//
// It runs two listeners:
//
//   - http://localhost:8080  — serves the debug UI plus a /info endpoint
//     that returns details of the last observed CONNECT request.
//   - https://localhost:12345/echo — a WebTransport echo endpoint that
//     echoes back any bidi stream, drains uni streams, and echoes datagrams.
//
// The browser pins the cert via serverCertificateHashes so no system trust
// store changes are needed.
package main

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	_ "embed"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"math/big"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/quic-go/quic-go/http3"

	"github.com/quic-go/webtransport-go"
)

//go:embed debug.html
var debugHTML string

type connectInfo struct {
	Method  string      `json:"method"`
	Proto   string      `json:"proto"`
	Host    string      `json:"host"`
	Path    string      `json:"path"`
	Origin  string      `json:"origin"`
	Headers http.Header `json:"headers"`
	At      time.Time   `json:"at"`
}

type lastConnect struct {
	mu sync.Mutex
	v  connectInfo
}

func (l *lastConnect) set(r *http.Request) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.v = connectInfo{
		Method:  r.Method,
		Proto:   r.Proto,
		Host:    r.Host,
		Path:    r.URL.Path,
		Origin:  r.Header.Get("Origin"),
		Headers: r.Header.Clone(),
		At:      time.Now(),
	}
}

func (l *lastConnect) get() connectInfo {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.v
}

func main() {
	tlsConf, err := getTLSConf(time.Now(), time.Now().Add(10*24*time.Hour))
	if err != nil {
		log.Fatal(err)
	}
	certHash := sha256.Sum256(tlsConf.Certificates[0].Leaf.Raw)

	var lc lastConnect

	go runHTTPServer(certHash, &lc)

	wmux := http.NewServeMux()
	s := webtransport.Server{
		H3: &http3.Server{
			TLSConfig: tlsConf,
			Addr:      "localhost:12345",
			Handler:   wmux,
		},
		CheckOrigin: func(r *http.Request) bool { return true },
	}
	webtransport.ConfigureHTTP3Server(s.H3)
	defer s.Close()

	wmux.HandleFunc("/echo", func(w http.ResponseWriter, r *http.Request) {
		lc.set(r)
		sess, err := s.Upgrade(w, r)
		if err != nil {
			log.Printf("upgrade failed: %s", err)
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		log.Printf("session up: proto=%q remote=%s", r.Proto, sess.RemoteAddr())
		runEcho(sess)
	})

	log.Printf("WebTransport echo at https://localhost:12345/echo")
	log.Printf("Debug UI at http://localhost:8080/")
	if err := s.ListenAndServe(); err != nil {
		log.Fatal(err)
	}
}

func runEcho(sess *webtransport.Session) {
	ctx := sess.Context()

	// Bidi: echo every byte back.
	go func() {
		for {
			stream, err := sess.AcceptStream(ctx)
			if err != nil {
				return
			}
			go func(s *webtransport.Stream) {
				defer s.Close()
				if _, err := io.Copy(s, s); err != nil && !errors.Is(err, io.EOF) {
					log.Printf("bidi copy: %s", err)
				}
			}(stream)
		}
	}()

	// Uni: drain into the void and report size.
	go func() {
		for {
			stream, err := sess.AcceptUniStream(ctx)
			if err != nil {
				return
			}
			go func(s *webtransport.ReceiveStream) {
				n, err := io.Copy(io.Discard, s)
				if err != nil && !errors.Is(err, io.EOF) {
					log.Printf("uni copy: %s", err)
				}
				log.Printf("uni stream drained: %d bytes", n)
			}(stream)
		}
	}()

	// Datagrams: echo back.
	for {
		data, err := sess.ReceiveDatagram(ctx)
		if err != nil {
			return
		}
		if err := sess.SendDatagram(data); err != nil {
			log.Printf("send datagram: %s", err)
			return
		}
	}
}

func runHTTPServer(certHash [32]byte, lc *lastConnect) {
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, _ *http.Request) {
		content := strings.ReplaceAll(debugHTML, "%%CERTHASH%%", formatByteSlice(certHash[:]))
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = w.Write([]byte(content))
	})
	mux.HandleFunc("/info", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Cache-Control", "no-store")
		_ = json.NewEncoder(w).Encode(lc.get())
	})
	if err := http.ListenAndServe("localhost:8080", mux); err != nil {
		log.Fatal(err)
	}
}

func getTLSConf(start, end time.Time) (*tls.Config, error) {
	cert, priv, err := generateCert(start, end)
	if err != nil {
		return nil, err
	}
	return http3.ConfigureTLSConfig(&tls.Config{
		Certificates: []tls.Certificate{{
			Certificate: [][]byte{cert.Raw},
			PrivateKey:  priv,
			Leaf:        cert,
		}},
	}), nil
}

func generateCert(start, end time.Time) (*x509.Certificate, *ecdsa.PrivateKey, error) {
	b := make([]byte, 8)
	if _, err := rand.Read(b); err != nil {
		return nil, nil, err
	}
	serial := int64(binary.BigEndian.Uint64(b))
	if serial < 0 {
		serial = -serial
	}
	tpl := &x509.Certificate{
		SerialNumber:          big.NewInt(serial),
		Subject:               pkix.Name{},
		NotBefore:             start,
		NotAfter:              end,
		IsCA:                  true,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth, x509.ExtKeyUsageServerAuth},
		KeyUsage:              x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign,
		BasicConstraintsValid: true,
	}
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, nil, err
	}
	der, err := x509.CreateCertificate(rand.Reader, tpl, tpl, &key.PublicKey, key)
	if err != nil {
		return nil, nil, err
	}
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		return nil, nil, err
	}
	return cert, key, nil
}

func formatByteSlice(b []byte) string {
	s := strings.ReplaceAll(fmt.Sprintf("%#v", b), "[]byte{", "[")
	return strings.ReplaceAll(s, "}", "]")
}
