// Scan Box: scan from a USB scanner into Paperless, run from one web page.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"
	_ "time/tzdata"

	"scanbox/internal/paperless"
	"scanbox/internal/scanner"
	"scanbox/internal/settings"
	"scanbox/internal/setup"
	"scanbox/internal/station"
	"scanbox/internal/web"
)

const (
	readHeaderTimeout = 10 * time.Second
	// readTimeout bounds reading a whole request; the largest is a form of 64 KiB. There is
	// no write timeout: the live updates stream for as long as a page is open.
	readTimeout     = 30 * time.Second
	idleTimeout     = 2 * time.Minute
	shutdownTimeout = 10 * time.Second
	healthTimeout   = 3 * time.Second
	loopback        = "127.0.0.1"
	// exitStuck ends the program when the scanner driver never returns: Docker starts it
	// again, with the driver loaded afresh. The open document is on disk.
	exitStuck = 3
)

// version is the release this program was built as, set by the image build from the git
// tag (-ldflags "-X main.version=0.1.0"); other builds say dev.
var version = "dev"

func main() {
	dataDir := flag.String("data", setup.ContainerDataFolder, "folder for settings and scans")
	listen := flag.String("listen", ":"+setup.Port, "address the page listens on")
	fake := flag.Bool("fake-scanner", false, "use a pretend scanner, to try the page without hardware")
	health := flag.Bool("health", false, "check that the page answers, for Docker's health check, and exit")
	showVersion := flag.Bool("version", false, "print the release this program was built as, and exit")
	flag.Parse()

	if *showVersion {
		fmt.Println(version)
		return
	}

	if *health {
		os.Exit(checkHealth(*listen))
	}
	if err := run(*dataDir, *listen, *fake); err != nil {
		log.Fatal(err)
	}
}

// checkHealth asks the running page for /healthz; the image has no curl.
func checkHealth(listen string) int {
	address, err := healthAddress(listen)
	if err != nil {
		log.Printf("-listen %q: %v", listen, err)
		return 1
	}
	client := http.Client{Timeout: healthTimeout}
	response, err := client.Get("http://" + address + "/healthz")
	if err != nil {
		return 1
	}
	response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return 1
	}
	return 0
}

// healthAddress is where the page answers: the address it listens on, or this machine
// when it listens on every address.
func healthAddress(listen string) (string, error) {
	host, port, err := net.SplitHostPort(listen)
	if err != nil {
		return "", err
	}
	if host == "" || net.ParseIP(host).IsUnspecified() {
		host = loopback
	}
	return net.JoinHostPort(host, port), nil
}

func run(dataDir, listen string, fake bool) error {
	token, tokenSource, err := setup.ReadSecret(setup.TokenVariable)
	if err != nil {
		log.Print(err)
	}
	password, passwordSource, passwordProblem := setup.ReadSecret(setup.PasswordVariable)
	if passwordProblem != nil {
		log.Print(passwordProblem)
	}
	access, lock := setup.PageAccess(passwordSource, passwordProblem)
	logAccess(access, lock)
	proxies, unreadable := setup.TrustedProxies()
	if len(unreadable) > 0 {
		log.Printf("%s: left out %q, which are neither addresses nor ranges", setup.TrustedProxyVariable, unreadable)
	}
	store, err := settings.Open(dataDir)
	if err != nil {
		return setup.DataAdvice(dataDir, err)
	}
	if store.Damaged != "" {
		log.Print(store.Damaged)
	}
	// A new install starts in the container's time zone, if it has one; after that the page decides.
	if zone := os.Getenv(setup.TimeZoneVariable); store.Fresh && zone != "" {
		if _, err := store.Set("time_zone", zone); err != nil {
			log.Printf("TZ=%s is not a time zone Scan Box knows; using %s", zone, store.Get().TimeZone)
		}
	}
	destination := paperless.Destination{Settings: store, Token: token}
	box, err := station.New(station.Config{DataDir: dataDir, Settings: store, Driver: chooseDriver(fake), Destination: destination, Stuck: stop})
	if err != nil {
		return setup.DataAdvice(dataDir, err)
	}
	page, err := web.New(web.Options{
		Version: version,
		DataDir: dataDir, Password: password, Access: access, Lock: lock, Hosts: setup.Hosts(), TrustedProxies: proxies,
		Station: box, Settings: store, Checker: destination,
		Facts: setup.Facts{
			TokenSource:    tokenSource,
			PasswordSource: passwordSource,
			DataIsVolume:   setup.DataIsVolume(dataDir),
		},
	})
	if err != nil {
		return setup.DataAdvice(dataDir, err)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	stationDone := make(chan struct{})
	go func() { defer close(stationDone); box.Run(ctx) }()
	if store.Get().PaperlessURL != "" && token != "" {
		go page.CheckPaperless(ctx)
	}
	go page.WatchPaperless(ctx)

	server := &http.Server{Addr: listen, Handler: page.Handler(), ReadHeaderTimeout: readHeaderTimeout, ReadTimeout: readTimeout, IdleTimeout: idleTimeout}
	go func() {
		<-ctx.Done()
		shutdown, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
		defer cancel()
		server.Shutdown(shutdown)
	}()
	log.Printf("Scan Box is listening on %s", listen)
	if err := server.ListenAndServe(); !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	<-stationDone
	return nil
}

// logAccess says in the log when the page is not behind its password: locked, or open to all.
func logAccess(access setup.Access, lock setup.Check) {
	switch access {
	case setup.Locked:
		log.Printf("the page is locked until it has a password it can use: %s", lock.Detail)
	case setup.Open:
		log.Printf("the page has no password and is open to everyone who can reach it, as %s asks", setup.AllowNoPasswordVariable)
	}
}

// stop ends the program for a scanner driver that never returned, so Docker starts it again.
func stop() {
	log.Print("stopping, so that Docker starts Scan Box again with the scanner driver loaded afresh")
	os.Exit(exitStuck)
}

func chooseDriver(fake bool) scanner.Driver {
	if fake {
		return scanner.NewFake()
	}
	return scanner.Default()
}
