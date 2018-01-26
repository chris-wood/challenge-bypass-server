package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/ioutil"
	"log"
	"net"
	"os"
	"time"

	"github.com/cloudflare/btd"
	"github.com/cloudflare/btd/crypto"
	"github.com/cloudflare/btd/metrics"
)

var (
	Version         = "dev"
	maxBackoffDelay = 1 * time.Second
	maxRequestSize  = int64(20 * 1024) // ~10kB is expected size for 100*base64([64]byte) + ~framing

	ErrEmptyKeyPath        = errors.New("key file path is empty")
	ErrNoSecretKey         = errors.New("server config does not contain a key")
	ErrRequestTooLarge     = errors.New("request too large to process")
	ErrUnrecognizedRequest = errors.New("received unrecognized request type")
	// Commitments are embedded straight into the extension for now
	ErrEmptyCommPath = errors.New("no commitment file path specified")

	errLog *log.Logger = log.New(os.Stderr, "[btd] ", log.LstdFlags|log.Lshortfile)
)

type Server struct {
	BindAddress  string `json:"bind_address,omitempty"`
	ListenPort   int    `json:"listen_port,omitempty"`
	MetricsPort  int    `json:"metrics_port,omitempty"`
	MaxTokens    int    `json:"max_tokens,omitempty"`
	KeyFilePath  string `json:"key_file_path"`
	CommFilePath string `json:"comm_file_path"`

	keys [][]byte      // a big-endian marshaled big.Int representing an elliptic curve scalar
	G    *crypto.Point // elliptic curve point representation of generator G
	H    *crypto.Point // elliptic curve point representation of commitment H to keys[0]
}

var DefaultServer = &Server{
	BindAddress: "127.0.0.1",
	ListenPort:  2416,
	MetricsPort: 2417,
	MaxTokens:   100,
}

func loadConfigFile(filePath string) (Server, error) {
	conf := *DefaultServer
	data, err := ioutil.ReadFile(filePath)
	if err != nil {
		return conf, err
	}
	err = json.Unmarshal(data, conf)
	if err != nil {
		return conf, err
	}
	return conf, nil
}

// return nil to exit without complaint, caller closes
func (c *Server) handle(conn *net.TCPConn) error {
	metrics.CounterConnections.Inc()

	// This is directly in the user's path, an overly slow connection should just fail
	conn.SetReadDeadline(time.Now().Add(100 * time.Millisecond))

	// Read the request but never more than a worst-case assumption
	var buf = new(bytes.Buffer)
	limitedConn := io.LimitReader(conn, maxRequestSize)
	_, err := io.Copy(buf, limitedConn)

	if err != nil {
		if opErr, ok := err.(*net.OpError); ok && opErr.Err.Error() == "i/o timeout" && buf.Len() > 0 {
			// then probably we just hit the read deadline, so try to unwrap anyway
		} else {
			metrics.CounterConnErrors.Inc()
			return err
		}
	}

	var wrapped btd.BlindTokenRequestWrapper
	var request btd.BlindTokenRequest

	err = json.Unmarshal(buf.Bytes(), &wrapped)
	if err != nil {
		metrics.CounterJsonError.Inc()
		return err
	}
	err = json.Unmarshal(wrapped.Request, &request)
	if err != nil {
		metrics.CounterJsonError.Inc()
		return err
	}

	switch request.Type {
	case btd.ISSUE:
		metrics.CounterIssueTotal.Inc()
		// use the first key as issue key
		err = btd.HandleIssue(conn, request, c.keys[0], c.G, c.H, c.MaxTokens)
		if err != nil {
			metrics.CounterIssueError.Inc()
			return err
		}
		return nil
	case btd.REDEEM:
		metrics.CounterRedeemTotal.Inc()
		err = btd.HandleRedeem(conn, request, wrapped.Host, wrapped.Path, c.keys)
		if err != nil {
			metrics.CounterRedeemError.Inc()
			conn.Write([]byte(err.Error())) // anything other than "success" counts as a VERIFY_ERROR
			return err
		}
		return nil
	default:
		errLog.Printf("unrecognized request type \"%s\"", request.Type)
		metrics.CounterUnknownRequestType.Inc()
		return ErrUnrecognizedRequest
	}
}

// loadKeys loads keys from the configured location
func (c *Server) loadKeys() error {
	if c.KeyFilePath == "" {
		return ErrEmptyKeyPath
	} else if c.CommFilePath == "" {
		return ErrEmptyCommPath
	}

	_, keys, err := crypto.ParseKeyFile(c.KeyFilePath)
	if err != nil {
		return err
	}
	c.keys = keys

	return nil
}

func (c *Server) ListenAndServe() error {
	if len(c.keys) == 0 {
		return ErrNoSecretKey
	}

	addr := fmt.Sprintf("%s:%d", c.BindAddress, c.ListenPort)
	tcpAddr, err := net.ResolveTCPAddr("tcp", addr)
	if err != nil {
		return err
	}
	listener, err := net.ListenTCP("tcp", tcpAddr)
	if err != nil {
		return err
	}
	defer listener.Close()
	errLog.Printf("blindsigmgmt starting, version: %v", Version)
	errLog.Printf("listening on %s", addr)

	// Initialize prometheus endpoint
	metricsAddr := fmt.Sprintf("%s:%d", c.BindAddress, c.MetricsPort)
	go func() {
		metrics.RegisterAndListen(metricsAddr, errLog)
	}()

	// Log errors without killing the entire server
	errorChannel := make(chan error)
	go func() {
		for err := range errorChannel {
			if err == nil {
				continue
			}
			errLog.Printf("%v", err)
		}
	}()

	// how long to wait for temporary net errors
	backoffDelay := 1 * time.Millisecond

	for {
		tcpConn, err := listener.AcceptTCP()
		if err != nil {
			if netErr, ok := err.(net.Error); ok {
				if netErr.Temporary() {
					// let's wait
					if backoffDelay > maxBackoffDelay {
						backoffDelay = maxBackoffDelay
					}
					time.Sleep(backoffDelay)
					backoffDelay = 2 * backoffDelay
				}
			}
			metrics.CounterConnErrors.Inc()
			errorChannel <- err
			continue
		}

		backoffDelay = 1 * time.Millisecond
		tcpConn.SetKeepAlive(true)
		tcpConn.SetKeepAlivePeriod(1 * time.Minute)

		go func() {
			errorChannel <- c.handle(tcpConn)
			tcpConn.Close()
		}()
	}
}

func main() {
	var configFile string
	var err error
	srv := *DefaultServer

	flag.StringVar(&configFile, "config", "", "local config file for development (overrides cli options)")
	flag.StringVar(&srv.BindAddress, "addr", "127.0.0.1", "address to listen on")
	flag.StringVar(&srv.KeyFilePath, "key", "", "path to the secret key file")
	flag.StringVar(&srv.CommFilePath, "comm", "", "path to the commitment file")
	flag.IntVar(&srv.ListenPort, "p", 2416, "port to listen on")
	flag.IntVar(&srv.MetricsPort, "m", 2417, "metrics port")
	flag.IntVar(&srv.MaxTokens, "maxtokens", 100, "maximum number of tokens issued per request")
	flag.Parse()

	if configFile != "" {
		srv, err = loadConfigFile(configFile)
		if err != nil {
			errLog.Fatal(err)
			return
		}
	}

	if configFile == "" && (srv.KeyFilePath == "" || srv.CommFilePath == "") {
		flag.Usage()
		return
	}

	err = srv.loadKeys()
	if err != nil {
		errLog.Fatal(err)
		return
	}

	// Get bytes for public commitment to private key
	GBytes, HBytes, err := crypto.ParseCommitmentFile(srv.CommFilePath)
	if err != nil {
		errLog.Fatal(err)
		return
	}

	// Retrieve the actual elliptic curve points for the commitment
	srv.G, srv.H, err = crypto.RetrieveCommPoints(GBytes, HBytes, srv.keys[0])
	if err != nil {
		errLog.Fatal(err)
		return
	}

	err = srv.ListenAndServe()

	if err != nil {
		errLog.Fatal(err)
		return
	}
}
