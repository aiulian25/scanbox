package web

import (
	"context"
	"net/url"
	"time"

	"scanbox/internal/clock"
	"scanbox/internal/paperless"
)

// probeInterval is how often Paperless is asked whether it answers, and which version it runs.
const probeInterval = time.Minute

// remoteState is what Paperless said the last time it was asked.
type remoteState struct {
	asked   time.Time
	info    paperless.ServerInfo
	problem string
}

// WatchPaperless asks Paperless how it is every probeInterval, and at once after its
// address changes, so the page shows what Paperless reports now.
func (s *Server) WatchPaperless(ctx context.Context) {
	ticker := time.NewTicker(probeInterval)
	defer ticker.Stop()
	for {
		s.probePaperless(ctx)
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		case <-s.probeNow:
		}
	}
}

func (s *Server) probePaperless(ctx context.Context) {
	info, err := s.options.Checker.Probe(ctx)
	state := remoteState{asked: s.options.Now(), info: info}
	if err != nil {
		state.problem = firstClause(err.Error())
	}
	s.mutex.Lock()
	s.remote = state
	s.mutex.Unlock()
	s.options.Station.Touch()
}

func (s *Server) askPaperlessNow() {
	select {
	case s.probeNow <- struct{}{}:
	default:
	}
}

// paperlessView is the line about Paperless at the foot of every page.
type paperlessView struct {
	Dot     string
	Summary string
	Host    string
}

func describePaperless(state remoteState, address string, writer clock.Clock) paperlessView {
	view := paperlessView{Host: hostOf(address)}
	switch {
	case address == "":
		view.Dot, view.Summary = "warn", "Paperless address not set"
	case state.asked.IsZero():
		view.Dot, view.Summary = "warn", "Paperless not asked yet"
	case state.problem != "":
		view.Dot, view.Summary = "error", state.problem+" · asked "+writer.Time(state.asked)
	default:
		view.Summary = "Paperless" + prefixed(" ", state.info.Version) + prefixed(" · API ", state.info.APIVersion) +
			" · answered " + writer.Time(state.asked)
	}
	return view
}

// prefixed puts a label before a value, or gives nothing when Paperless didn't report it.
func prefixed(label, value string) string {
	if value == "" {
		return ""
	}
	return label + value
}

func hostOf(address string) string {
	parsed, err := url.Parse(address)
	if err != nil || parsed.Host == "" {
		return address
	}
	return parsed.Host
}
