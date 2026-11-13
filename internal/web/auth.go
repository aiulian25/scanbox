package web

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"log"
	"net"
	"net/http"
	"net/netip"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"scanbox/internal/setup"
)

const (
	sessionCookie      = "scanbox_session"
	sessionLifetime    = 30 * 24 * time.Hour
	sessionKeyFile     = "session.key"
	sessionKeyBytes    = 32
	keyFilePermissions = 0o600
	// generationFile holds how many times everyone was signed out; it is part of every
	// session's signature, so raising it ends them all.
	generationFile  = "session.generation"
	failurePause    = time.Second
	failureWindow   = time.Minute
	failuresAllowed = 5
	forwardedFor    = "X-Forwarded-For"
	forwardedProto  = "X-Forwarded-Proto"
	https           = "https"
)

var errTooManyTries = errors.New("too many wrong passwords; wait a minute and try again")

// guard is the page's sign-in. It lets in whoever signs in with the password, or everyone
// when the container asks for a page without one. In every other case, such as a password
// that was meant to be set but isn't there, nobody gets in.
type guard struct {
	password []byte
	access   setup.Access
	key      []byte
	// generationPath is where the count of sign-outs is kept.
	generationPath string
	// proxies are the reverse proxies whose forwarded headers are believed.
	proxies []netip.Prefix
	now     func() time.Time

	mutex      sync.Mutex
	generation uint64
	failures   map[string][]time.Time
}

// newGuard keeps the session key in the data folder, so sign-ins survive a restart.
// The key is mixed with the password, so changing the password signs everyone out.
func newGuard(dataDir, password string, access setup.Access, proxies []netip.Prefix, now func() time.Time) (*guard, error) {
	path := filepath.Join(dataDir, sessionKeyFile)
	key, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		key = make([]byte, sessionKeyBytes)
		if _, err := rand.Read(key); err != nil {
			return nil, err
		}
		err = os.WriteFile(path, key, keyFilePermissions)
	}
	if err != nil {
		return nil, err
	}
	generationPath := filepath.Join(dataDir, generationFile)
	generation, err := readGeneration(generationPath)
	if err != nil {
		return nil, err
	}
	passwordHash := sha256.Sum256([]byte(password))
	mixed := hmac.New(sha256.New, key)
	mixed.Write(passwordHash[:])
	return &guard{password: []byte(password), access: access, key: mixed.Sum(nil), generationPath: generationPath, proxies: proxies,
		now: now, generation: generation, failures: map[string][]time.Time{}}, nil
}

// readGeneration reads the count of sign-outs; none yet is 0.
func readGeneration(path string) (uint64, error) {
	content, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	return strconv.ParseUint(strings.TrimSpace(string(content)), 10, 64)
}

// endSessions ends every session, on every device: a copy of a cookie stops working too.
// The count is kept on disk, so a restart doesn't bring the sessions back.
func (g *guard) endSessions() error {
	g.mutex.Lock()
	defer g.mutex.Unlock()
	g.generation++
	temporary := g.generationPath + ".new"
	if err := os.WriteFile(temporary, []byte(strconv.FormatUint(g.generation, 10)), keyFilePermissions); err != nil {
		return err
	}
	return os.Rename(temporary, g.generationPath)
}

// enabled says whether people must sign in.
func (g *guard) enabled() bool { return len(g.password) > 0 && g.access != setup.Locked }

func (g *guard) open() bool { return len(g.password) == 0 && g.access == setup.Open }

// locked says whether nobody may use the page: neither a password to check, nor leave to
// do without one.
func (g *guard) locked() bool { return !g.enabled() && !g.open() }

// signature signs a session's expiry together with the count of sign-outs.
func (g *guard) signature(expiry string) string {
	g.mutex.Lock()
	generation := g.generation
	g.mutex.Unlock()
	mac := hmac.New(sha256.New, g.key)
	mac.Write([]byte(strconv.FormatUint(generation, 10) + "." + expiry))
	return base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
}

func (g *guard) signedIn(request *http.Request) bool {
	if g.locked() {
		return false
	}
	if g.open() {
		return true
	}
	cookie, err := request.Cookie(sessionCookie)
	if err != nil {
		return false
	}
	expiry, signature, found := strings.Cut(cookie.Value, ".")
	if !found || !hmac.Equal([]byte(signature), []byte(g.signature(expiry))) {
		return false
	}
	seconds, err := strconv.ParseInt(expiry, 10, 64)
	return err == nil && g.now().Before(time.Unix(seconds, 0))
}

// signIn checks the password and, if it is right, sets the session cookie.
func (g *guard) signIn(writer http.ResponseWriter, request *http.Request, attempt string) (bool, error) {
	address := g.clientAddress(request)
	if g.lockedOut(address) {
		log.Printf("sign-in: refused from %s, locked out after %d wrong passwords", address, failuresAllowed)
		return false, errTooManyTries
	}
	if subtle.ConstantTimeCompare([]byte(attempt), g.password) != 1 {
		log.Printf("sign-in: wrong password from %s", address)
		if g.recordFailure(address) == failuresAllowed {
			log.Printf("sign-in: %s locked out for %v after %d wrong passwords", address, failureWindow, failuresAllowed)
		}
		time.Sleep(failurePause)
		return false, nil
	}
	log.Printf("sign-in from %s", address)
	expiry := strconv.FormatInt(g.now().Add(sessionLifetime).Unix(), 10)
	http.SetCookie(writer, &http.Cookie{
		Name:     sessionCookie,
		Value:    expiry + "." + g.signature(expiry),
		Path:     "/",
		MaxAge:   int(sessionLifetime.Seconds()),
		HttpOnly: true,
		Secure:   g.isHTTPS(request),
		SameSite: http.SameSiteStrictMode,
	})
	return true, nil
}

// signOut ends every session and removes this browser's cookie.
func (g *guard) signOut(writer http.ResponseWriter) error {
	http.SetCookie(writer, &http.Cookie{Name: sessionCookie, Value: "", Path: "/", MaxAge: -1, HttpOnly: true, SameSite: http.SameSiteStrictMode})
	return g.endSessions()
}

func (g *guard) lockedOut(address string) bool {
	g.mutex.Lock()
	defer g.mutex.Unlock()
	recent := g.failures[address][:0]
	for _, failure := range g.failures[address] {
		if g.now().Sub(failure) < failureWindow {
			recent = append(recent, failure)
		}
	}
	g.failures[address] = recent
	return len(recent) >= failuresAllowed
}

// recordFailure notes a wrong password and gives how many that address has made lately.
func (g *guard) recordFailure(address string) int {
	g.mutex.Lock()
	defer g.mutex.Unlock()
	g.failures[address] = append(g.failures[address], g.now())
	return len(g.failures[address])
}

// clientAddress is who is signing in: the connection's address or, behind a trusted
// proxy, the address the proxy saw, which it added last to X-Forwarded-For. So a lockout
// applies to one client, not to everyone behind the proxy.
func (g *guard) clientAddress(request *http.Request) string {
	host, _, err := net.SplitHostPort(request.RemoteAddr)
	if err != nil {
		host = request.RemoteAddr
	}
	forwarded := request.Header.Values(forwardedFor)
	if !g.fromProxy(request) || len(forwarded) == 0 {
		return host
	}
	hops := strings.Split(forwarded[len(forwarded)-1], ",")
	client, err := netip.ParseAddr(strings.TrimSpace(hops[len(hops)-1]))
	if err != nil {
		return host
	}
	return client.String()
}

// isHTTPS says whether the browser reached the page over https: directly, or through a
// trusted proxy that says so. Anyone else could write X-Forwarded-Proto.
func (g *guard) isHTTPS(request *http.Request) bool {
	return request.TLS != nil || g.fromProxy(request) && request.Header.Get(forwardedProto) == https
}

// fromProxy says whether the request came straight from a trusted proxy.
func (g *guard) fromProxy(request *http.Request) bool {
	connection, err := netip.ParseAddrPort(request.RemoteAddr)
	if err != nil {
		return false
	}
	address := connection.Addr().Unmap()
	return slices.ContainsFunc(g.proxies, func(proxy netip.Prefix) bool { return proxy.Contains(address) })
}

// sameOrigin refuses changes sent from other sites, on top of the SameSite cookie.
func sameOrigin(request *http.Request) bool {
	origin := request.Header.Get("Origin")
	if origin == "" {
		origin = request.Header.Get("Referer")
	}
	if origin == "" {
		return false
	}
	_, rest, found := strings.Cut(origin, "://")
	if !found {
		return false
	}
	host, _, _ := strings.Cut(rest, "/")
	return host == request.Host
}
