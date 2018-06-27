package server

import (
	"crypto"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"github.com/hlandau/buildinfo"
	"github.com/hlandau/xlog"
	"github.com/miekg/dns"
	"github.com/namecoin/ncdns/backend"
	"github.com/namecoin/ncdns/namecoin"
	"gopkg.in/hlandau/madns.v1"
)

var log, Log = xlog.New("ncdns.server")

type Server struct {
	cfg Config

	engine       madns.Engine
	namecoinConn namecoin.Conn

	mux         *dns.ServeMux
	udpServer   *dns.Server
	udpConn     *net.UDPConn
	tcpServer   *dns.Server
	tcpListener net.Listener
	wgStart     sync.WaitGroup
}

type Config struct {
	Bind           string `default:":53" usage:"Address to bind to (e.g. 0.0.0.0:53)"`
	PublicKey      string `default:"" usage:"Path to the DNSKEY KSK public key file"`
	PrivateKey     string `default:"" usage:"Path to the KSK's corresponding private key file"`
	ZonePublicKey  string `default:"" usage:"Path to the DNSKEY ZSK public key file; if one is not specified, a temporary one is generated on startup and used only for the duration of that process"`
	ZonePrivateKey string `default:"" usage:"Path to the ZSK's corresponding private key file"`

	NamecoinRPCUsername   string `default:"" usage:"Namecoin RPC username"`
	NamecoinRPCPassword   string `default:"" usage:"Namecoin RPC password"`
	NamecoinRPCAddress    string `default:"127.0.0.1:8336" usage:"Namecoin RPC server address"`
	NamecoinRPCCookiePath string `default:"" usage:"Namecoin RPC cookie path (if set, used instead of password)"`
	CacheMaxEntries       int    `default:"100" usage:"Maximum name cache entries"`
	SelfName              string `default:"" usage:"The FQDN of this nameserver. If empty, a pseudo-hostname is generated."`
	SelfIP                string `default:"127.127.127.127" usage:"The canonical IP address for this service"`

	HTTPListenAddr string `default:"" usage:"Address for webserver to listen at (default: disabled)"`

	CanonicalSuffix      string `default:"bit" usage:"Suffix to advertise via HTTP"`
	CanonicalNameservers string `default:"" usage:"Comma-separated list of nameservers to use for NS records. If blank, SelfName (or autogenerated pseudo-hostname) is used."`
	canonicalNameservers []string
	Hostmaster           string `default:"" usage:"Hostmaster e. mail address"`
	VanityIPs            string `default:"" usage:"Comma separated list of IP addresses to place in A/AAAA records at the zone apex (default: don't add any records)"`
	vanityIPs            []net.IP
	TplSet               string `default:"std" usage:"The template set to use"`
	TplPath              string `default:"" usage:"The path to the tpl directory (empty: autodetect)"`

	ConfigDir string // path to interpret filenames relative to
}

func (cfg *Config) cpath(s string) string {
	return filepath.Join(cfg.ConfigDir, s)
}

var ncdnsVersion string

func New(cfg *Config) (s *Server, err error) {
	ncdnsVersion = buildinfo.VersionSummary("github.com/namecoin/ncdns", "ncdns")

	s = &Server{
		cfg: *cfg,
		namecoinConn: namecoin.Conn{
			Username: cfg.NamecoinRPCUsername,
			Password: cfg.NamecoinRPCPassword,
			Server:   cfg.NamecoinRPCAddress,
		},
	}

	if s.cfg.NamecoinRPCCookiePath != "" {
		s.namecoinConn.GetAuth = cookieRetriever(s.cfg.NamecoinRPCCookiePath)
	}

	if s.cfg.CanonicalNameservers != "" {
		s.cfg.canonicalNameservers = strings.Split(s.cfg.CanonicalNameservers, ",")
		for i := range s.cfg.canonicalNameservers {
			s.cfg.canonicalNameservers[i] = dns.Fqdn(s.cfg.canonicalNameservers[i])
		}
	}

	if s.cfg.VanityIPs != "" {
		vanityIPs := strings.Split(s.cfg.VanityIPs, ",")
		for _, ips := range vanityIPs {
			ip := net.ParseIP(ips)
			if ip == nil {
				return nil, fmt.Errorf("Couldn't parse IP: %s", ips)
			}
			s.cfg.vanityIPs = append(s.cfg.vanityIPs, ip)
		}
	}

	b, err := backend.New(&backend.Config{
		NamecoinConn:         s.namecoinConn,
		CacheMaxEntries:      cfg.CacheMaxEntries,
		SelfIP:               cfg.SelfIP,
		Hostmaster:           cfg.Hostmaster,
		CanonicalNameservers: s.cfg.canonicalNameservers,
		VanityIPs:            s.cfg.vanityIPs,
	})
	if err != nil {
		return
	}

	ecfg := &madns.EngineConfig{
		Backend:       b,
		VersionString: ncdnsVersion,
	}

	// key setup
	if cfg.PublicKey != "" {
		ecfg.KSK, ecfg.KSKPrivate, err = s.loadKey(cfg.PublicKey, cfg.PrivateKey)
		if err != nil {
			return nil, err
		}
	}

	if cfg.ZonePublicKey != "" {
		ecfg.ZSK, ecfg.ZSKPrivate, err = s.loadKey(cfg.ZonePublicKey, cfg.ZonePrivateKey)
		if err != nil {
			return nil, err
		}
	}

	if ecfg.KSK != nil && ecfg.ZSK == nil {
		return nil, fmt.Errorf("Must specify ZSK if KSK is specified")
	}

	s.engine, err = madns.NewEngine(ecfg)
	if err != nil {
		return
	}

	s.mux = dns.NewServeMux()
	s.mux.Handle(".", s.engine)

	tcpAddr, err := net.ResolveTCPAddr("tcp", s.cfg.Bind)
	if err != nil {
		return
	}

	s.tcpListener, err = net.ListenTCP("tcp", tcpAddr)
	if err != nil {
		return
	}

	udpAddr, err := net.ResolveUDPAddr("udp", s.cfg.Bind)
	if err != nil {
		return
	}

	s.udpConn, err = net.ListenUDP("udp", udpAddr)
	if err != nil {
		return
	}

	if cfg.HTTPListenAddr != "" {
		err = webStart(cfg.HTTPListenAddr, s)
		if err != nil {
			return
		}
	}

	return
}

func (s *Server) loadKey(fn, privateFn string) (k *dns.DNSKEY, privatek crypto.PrivateKey, err error) {
	fn = s.cfg.cpath(fn)
	privateFn = s.cfg.cpath(privateFn)

	f, err := os.Open(fn)
	if err != nil {
		return
	}

	rr, err := dns.ReadRR(f, fn)
	if err != nil {
		return
	}

	k, ok := rr.(*dns.DNSKEY)
	if !ok {
		err = fmt.Errorf("Loaded record from key file, but it wasn't a DNSKEY")
		return
	}

	privatef, err := os.Open(privateFn)
	if err != nil {
		return
	}

	privatek, err = k.ReadPrivateKey(privatef, privateFn)
	log.Fatale(err)

	return
}

func (s *Server) Start() error {
	s.wgStart.Add(2)
	s.udpServer = s.runListener("udp")
	s.tcpServer = s.runListener("tcp")
	s.wgStart.Wait()
	log.Info("Listeners started")
	return nil
}

func (s *Server) doRunListener(ds *dns.Server) {
	err := ds.ActivateAndServe()
	log.Fatale(err)
}

func (s *Server) runListener(net string) *dns.Server {
	ds := &dns.Server{
		Addr:    s.cfg.Bind,
		Net:     net,
		Handler: s.mux,
		NotifyStartedFunc: func() {
			s.wgStart.Done()
		},
	}
	switch net {
	case "tcp":
		ds.Listener = s.tcpListener
	case "udp":
		ds.PacketConn = s.udpConn
	default:
		panic("unreachable")
	}

	go s.doRunListener(ds)
	return ds
}

func (s *Server) Stop() error {
	return nil // TODO
}
