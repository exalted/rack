package router

import (
	"crypto/tls"
	"fmt"
	"math/rand"
	"net"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/convox/rack/pkg/helpers"
	"github.com/miekg/dns"
	"golang.org/x/crypto/acme/autocert"
	am "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
)

const (
	idleCheck   = 1 * time.Minute
	idleTimeout = 60 * time.Minute
)

var (
	targetParser = regexp.MustCompile(`^([^.]+)\.([^.]+)\.svc\.cluster\.local$`)
)

type Router struct {
	Cluster kubernetes.Interface
	DNS     Server
	HTTP    Server
	HTTPS   Server

	certs   sync.Map
	ip      string
	prefix  string
	service string
	storage Storage
}

type Server interface {
	ListenAndServe() error
}

func init() {
	rand.Seed(time.Now().UTC().UnixNano())
}

func New() (*Router, error) {
	r := &Router{
		certs: sync.Map{},
	}

	switch os.Getenv("BACKEND") {
	case "dynamodb":
		r.storage = NewStorageDynamo(os.Getenv("ROUTER_ROUTES"))
	default:
		r.storage = NewStorageMemory()
	}

	c, err := rest.InClusterConfig()
	if err != nil {
		return nil, err
	}

	kc, err := kubernetes.NewForConfig(c)
	if err != nil {
		return nil, err
	}

	r.Cluster = kc

	if err := r.setupDNS(); err != nil {
		return nil, err
	}

	if err := r.setupHTTP(); err != nil {
		return nil, err
	}

	ic, err := NewIngressController(r)
	if err != nil {
		return nil, err
	}

	go ic.Run()

	if parts := strings.Split(os.Getenv("POD_IP"), "."); len(parts) > 2 {
		r.prefix = fmt.Sprintf("%s.%s.", parts[0], parts[1])
	}

	if host := os.Getenv("SERVICE_HOST"); host != "" {
		for {
			if ips, err := net.LookupIP(host); err == nil && len(ips) > 0 {
				r.service = ips[0].String()
				break
			}

			time.Sleep(1 * time.Second)
		}
	}

	s, err := kc.CoreV1().Services("convox-system").Get("router", am.GetOptions{})
	if err != nil {
		return nil, err
	}

	if len(s.Status.LoadBalancer.Ingress) > 0 && s.Status.LoadBalancer.Ingress[0].Hostname == "localhost" {
		r.ip = "127.0.0.1"
	} else {
		r.ip = s.Spec.ClusterIP
	}

	fmt.Printf("ns=router at=new ip=%s prefix=%s service=%s\n", r.ip, r.prefix, r.service)

	return r, nil
}

func (r *Router) Serve() error {
	ch := make(chan error, 1)

	go serve(ch, r.DNS)
	go serve(ch, r.HTTP)
	go serve(ch, r.HTTPS)

	go r.idleTicker()

	return <-ch
}

func (r *Router) ExternalIP(remote net.Addr) string {
	if strings.HasPrefix(remote.String(), r.prefix) {
		return r.service
	}

	return r.ip
}

func (r *Router) RequestBegin(host string) error {
	idle, err := r.storage.IdleGet(host)
	if err != nil {
		return fmt.Errorf("could not fetch idle status: %s", err)
	}

	if idle {
		if err := r.HostUnidle(host); err != nil {
			return fmt.Errorf("could not unidle: %s", err)
		}
	}

	return r.storage.RequestBegin(host)
}

func (r *Router) RequestEnd(host string) error {
	return r.storage.RequestEnd(host)
}

func (r *Router) Route(host string) (string, error) {
	ts, err := r.TargetList(host)
	if err != nil {
		return "", fmt.Errorf("no backends available")
	}

	if len(ts) < 1 {
		return "", fmt.Errorf("no backends available")
	}

	return ts[rand.Intn(len(ts))], nil
}

func (r *Router) TargetAdd(host, target string) error {
	fmt.Printf("ns=router at=target.add host=%q target=%q\n", host, target)

	if err := r.storage.TargetAdd(host, target); err != nil {
		return err
	}

	idle, err := r.HostIdleStatus(host)
	if err != nil {
		return err
	}

	if err := r.storage.IdleSet(host, idle); err != nil {
		return err
	}

	return nil
}

func (r *Router) TargetList(host string) ([]string, error) {
	return r.storage.TargetList(host)
}

func (r *Router) TargetRemove(host, target string) error {
	fmt.Printf("ns=router at=target.delete host=%q target=%q\n", host, target)

	return r.storage.TargetRemove(host, target)
}

func (r *Router) Upstream() (string, error) {
	cc, err := dns.ClientConfigFromFile("/etc/resolv.conf")
	if err != nil {
		return "", err
	}

	if len(cc.Servers) < 1 {
		return "", fmt.Errorf("no upstream dns")
	}

	return fmt.Sprintf("%s:53", cc.Servers[0]), nil
}

func (r *Router) generateCertificateAutocert(m *autocert.Manager) func(*tls.ClientHelloInfo) (*tls.Certificate, error) {
	return func(hello *tls.ClientHelloInfo) (*tls.Certificate, error) {
		if hello.ServerName == "" {
			return helpers.CertificateSelfSigned("convox")
		}

		return m.GetCertificate(hello)
	}
}

func (r *Router) generateCertificateCA(hello *tls.ClientHelloInfo) (*tls.Certificate, error) {
	host := hello.ServerName

	v, ok := r.certs.Load(host)
	if ok {
		if c, ok := v.(tls.Certificate); ok {
			return &c, nil
		}
	}

	ca, err := r.ca()
	if err != nil {
		return nil, err
	}

	c, err := helpers.CertificateCA(host, ca)
	if err != nil {
		return nil, err
	}

	r.certs.Store(host, *c)

	return c, nil
}

func (r *Router) setupDNS() error {
	conn, err := net.ListenPacket("udp", ":5453")
	if err != nil {
		return err
	}

	dns, err := NewDNS(conn, r)
	if err != nil {
		return err
	}

	r.DNS = dns

	return nil
}

func (r *Router) setupHTTP() error {
	if os.Getenv("AUTOCERT") == "true" {
		return r.setupHTTPAutocert()
	}

	ln, err := tls.Listen("tcp", ":443", &tls.Config{
		GetCertificate: r.generateCertificateCA,
	})
	if err != nil {
		return err
	}

	https, err := NewHTTP(ln, r)
	if err != nil {
		return err
	}

	r.HTTPS = https

	r.HTTP = &http.Server{Addr: ":80", Handler: redirectHTTPS(https.ServeHTTP)}

	return nil
}

func (r *Router) setupHTTPAutocert() error {
	m := &autocert.Manager{
		Cache:  NewCacheDynamo(os.Getenv("ROUTER_CACHE")),
		Prompt: autocert.AcceptTOS,
	}

	ln, err := tls.Listen("tcp", fmt.Sprintf(":443"), &tls.Config{
		GetCertificate: r.generateCertificateAutocert(m),
	})
	if err != nil {
		return err
	}

	https, err := NewHTTP(ln, r)
	if err != nil {
		return err
	}

	r.HTTPS = https

	r.HTTP = &http.Server{Addr: ":80", Handler: m.HTTPHandler(redirectHTTPS(https.ServeHTTP))}

	return nil
}

func parseTarget(target string) (string, string, bool) {
	u, err := url.Parse(target)
	if err != nil {
		return "", "", false
	}

	if m := targetParser.FindStringSubmatch(u.Hostname()); len(m) == 3 {
		return m[1], m[2], true
	}

	return "", "", false
}

func redirectHTTPS(fn http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-Forwarded-Proto") == "https" {
			fn(w, r)
			return
		}

		target := url.URL{Scheme: "https", Host: r.Host, Path: r.URL.Path, RawQuery: r.URL.RawQuery}

		http.Redirect(w, r, target.String(), http.StatusMovedPermanently)
	}
}

func serve(ch chan error, s Server) {
	ch <- s.ListenAndServe()
}
