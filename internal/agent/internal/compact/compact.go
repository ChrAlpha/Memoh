// Package compact decides when a conversation has outgrown its context window
// and runs the compaction that shortens it: before a turn, between the model
// calls of a run, and in the background after one. It also keeps a session's
// compaction and the runs that read its history from overlapping.
package compact

import (
	"context"
	"log/slog"
	"net"
	"net/http"
	"sync"
	"time"

	"github.com/felinics/memoh/internal/agent/context/compaction"
	"github.com/felinics/memoh/internal/agent/internal/transcript"
	dbstore "github.com/felinics/memoh/internal/db/store"
	"github.com/felinics/memoh/internal/models"
	"github.com/felinics/memoh/internal/settings"
)

// Settings is the part of the bot settings store that holds a bot's
// compaction configuration.
type Settings interface {
	GetBot(ctx context.Context, botID string) (settings.Settings, error)
}

// Runner is the compaction engine.
type Runner interface {
	RunCompactionSync(context.Context, compaction.TriggerConfig) (compaction.Result, error)
}

// Deps is everything New needs.
type Deps struct {
	// Logger receives the compactor's own records.
	Logger *slog.Logger
	// Settings holds a bot's compaction settings.
	Settings Settings
	// Models resolves the summarizer model.
	Models *models.Service
	// Queries is the store the summarizer's model and credential policy reads
	// through: those resolvers take the whole store, so it cannot be narrowed
	// here. The compactor itself reads one row of it, a session's latest
	// context lifecycle.
	Queries dbstore.Queries
	// Transcript reads the history a compaction is judged against and rebuilt
	// from.
	Transcript *transcript.Reader
	// Runner runs one compaction; without it nothing is compacted.
	Runner Runner
	// HTTPClient carries the summarizer's requests; nil builds the default,
	// which waits for a cold summarizer's first byte.
	HTTPClient *http.Client
}

// Compactor judges context pressure and runs compactions.
type Compactor struct {
	logger     *slog.Logger
	settings   Settings
	models     *models.Service
	queries    dbstore.Queries
	transcript *transcript.Reader
	runner     runner
	httpClient *http.Client

	// mu guards the session gates, which stand for the compactions and the
	// runs reading history right now.
	mu    sync.Mutex
	gates map[string]*sessionGate
}

// New returns a Compactor. Without a Runner it is built with a stand-in that
// compacts nothing.
func New(d Deps) *Compactor {
	c := &Compactor{
		logger:     d.Logger,
		settings:   d.Settings,
		models:     d.Models,
		queries:    d.Queries,
		transcript: d.Transcript,
		runner:     noRunner{},
		httpClient: d.HTTPClient,
		gates:      map[string]*sessionGate{},
	}
	if d.Runner != nil {
		c.runner = activeRunner{d.Runner}
	}
	if c.httpClient == nil {
		c.httpClient = defaultHTTPClient()
	}
	return c
}

// defaultHTTPClient is the summarizer's client. The summarizer sends one large
// cold prompt whose first byte routinely takes 60-93s upstream; an interactive
// 30s header deadline would kill every automatic pass while manual /compact
// (SDK default client) works.
func defaultHTTPClient() *http.Client {
	transport := &http.Transport{
		DialContext: (&net.Dialer{
			Timeout:   30 * time.Second,
			KeepAlive: 30 * time.Second,
		}).DialContext,
		TLSHandshakeTimeout:   10 * time.Second,
		ResponseHeaderTimeout: 3 * time.Minute,
		MaxIdleConns:          100,
		MaxIdleConnsPerHost:   10,
		IdleConnTimeout:       90 * time.Second,
	}
	return &http.Client{Transport: transport, Timeout: 10 * time.Minute}
}

// runner is the compaction engine as the compactor holds it: present, or a
// stand-in that is not.
type runner interface {
	Runner
	enabled() bool
}

type activeRunner struct{ Runner }

func (activeRunner) enabled() bool { return true }

type noRunner struct{}

func (noRunner) enabled() bool { return false }

func (noRunner) RunCompactionSync(context.Context, compaction.TriggerConfig) (compaction.Result, error) {
	return compaction.Result{Status: compaction.StatusNoop, Reason: compaction.ReasonNoModel}, nil
}
