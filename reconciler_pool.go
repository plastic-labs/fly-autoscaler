package fas

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/prometheus/client_golang/prometheus"
)

const (
	DefaultConcurrency            = 1
	DefaultReconcileTimeout       = 30 * time.Second
	DefaultReconcileInterval      = 15 * time.Second
	DefaultAppListRefreshInterval = 60 * time.Second
	DefaultProcessGroup           = "app"
)

// ReconcilerPool represents a set of reconcilers that act as a worker pool.
//
// This is used to distribute scaling across multiple applications while also
// limiting the maximum concurrency allowed by the scaler.
type ReconcilerPool struct {
	flyClient   FlyClient
	reconcilers []*Reconciler

	wg     sync.WaitGroup
	ctx    context.Context
	cancel context.CancelCauseFunc

	ch    chan appInfo // work queue
	orgID string       // cached organization id
	apps  struct {
		sync.Mutex
		m map[string]appInfo
	}

	// Time allowed to perform reconciliation for a single app.
	ReconcileTimeout time.Duration

	// Frequency to run the reconciliation loop for each app.
	ReconcileInterval time.Duration

	// Frequency to update the list of matching apps when using wildcards.
	AppListRefreshInterval time.Duration

	// Name of application to scale. Supports wildcards for multiple apps.
	// All applications must be in the same org.
	AppName string

	// Organization slug. Required if app name is a wildcard.
	OrganizationSlug string

	// NewFlapsClient is a constructor for building a FLAPS client for a given app.
	NewFlapsClient NewFlapsClientFunc

	// NewReconciler is a constructor for building reconcilers.
	// Called one or more times on Open().
	NewReconciler func() *Reconciler

	// Shared stats for all reconcilers.
	Stats ReconcilerStats
}

// NewReconcilerPool returns a new instance of ReconcilerPool.
func NewReconcilerPool(flyClient FlyClient, concurrency int) *ReconcilerPool {
	if concurrency < 1 {
		concurrency = 1
	}

	p := &ReconcilerPool{
		flyClient:   flyClient,
		reconcilers: make([]*Reconciler, concurrency),
		ch:          make(chan appInfo),

		ReconcileTimeout:       DefaultReconcileTimeout,
		ReconcileInterval:      DefaultReconcileInterval,
		AppListRefreshInterval: DefaultAppListRefreshInterval,
	}
	p.ctx, p.cancel = context.WithCancelCause(context.Background())
	p.apps.m = make(map[string]appInfo)

	return p
}

func (p *ReconcilerPool) Open() error {
	if p.AppName == "" {
		return fmt.Errorf("app name required")
	}
	if p.NewFlapsClient == nil {
		return fmt.Errorf("flaps client constructor required")
	}

	// Instantiate reconcilers.
	for i := range p.reconcilers {
		r := p.NewReconciler()
		r.Stats = &p.Stats // share the same stats object
		p.reconcilers[i] = r
	}

	// Limit concurrency to 1 if we only have a single app to manage.
	appNameHasWildcard := strings.Contains(p.AppName, "*")
	if !appNameHasWildcard {
		p.reconcilers = []*Reconciler{p.reconcilers[0]}
	}

	// We need the organization slug to fetch the list of app names so
	// ensure we have it if the app name uses a wildcard.
	if appNameHasWildcard && p.OrganizationSlug == "" {
		return fmt.Errorf("organization required if app name uses a wildcard")
	}

	// Start each reconciler in a separate goroutine and wait for work.
	p.wg.Add(len(p.reconcilers))
	for _, r := range p.reconcilers {
		r := r
		go func() { defer p.wg.Done(); p.monitorReconciler(p.ctx, r) }()
	}

	// If the app name does not contain a wildcard, set it as the value list
	// and have it push
	if !appNameHasWildcard {
		client, err := p.NewFlapsClient(context.Background(), p.AppName)
		if err != nil {
			return fmt.Errorf("cannot initialize flaps client: %w", err)
		}
		p.apps.m[p.AppName] = appInfo{
			name:   p.AppName,
			client: client,
		}

		p.wg.Add(1)
		go func() { defer p.wg.Done(); p.monitorWorkQueueGenerator(p.ctx) }()
	} else {
		// If there is wildcard then we need to kick off the app list monitor
		// first. Once we have a set of app names then we can kick off the
		// work queue.
		p.wg.Add(1)
		go func() { defer p.wg.Done(); p.monitorAppNameRefresh(p.ctx) }()
	}

	return nil
}

// Close stops all processing of the pool and underlying reconcilers.
// Only returns once all reconcilers have finished processing.
func (p *ReconcilerPool) Close() error {
	p.cancel(errReconcilerPoolClosing)
	p.wg.Wait()
	return nil
}

// monitorWorkQueueGenerator pushes all apps into the work queue on an interval.
func (p *ReconcilerPool) monitorWorkQueueGenerator(ctx context.Context) {
	ticker := time.NewTicker(p.ReconcileInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			// Fetch the app list under lock.
			p.apps.Lock()
			m := p.apps.m
			p.apps.Unlock()

			// Push all app names into the work queue.
			for _, info := range m {
				select {
				case <-ctx.Done():
					return
				case p.ch <- info:
				}
			}
		}
	}
}

// monitorAppNameRefresh runs in the background and periodically refreshes the
// list of apps to monitor. This will kick off another goroutine to push the
// current list of names into the work queue once obtained.
func (p *ReconcilerPool) monitorAppNameRefresh(ctx context.Context) {
	ticker := time.NewTicker(p.AppListRefreshInterval)
	defer ticker.Stop()

	var initialized bool
	for {
		// slog.Info("monitorAppNameRefresh: calling updateAppNameList")
		if err := p.updateAppNameList(ctx); err != nil {
			slog.Error("app list update failed", slog.Any("err", err))
		}
		// slog.Info("monitorAppNameRefresh: updateAppNameList call completed")

		// Start the work generator once we have our initial list.
		if !initialized {
			// slog.Info("monitorAppNameRefresh: first run complete, starting monitorWorkQueueGenerator")
			initialized = true
			p.wg.Add(1)
			go func() { defer p.wg.Done(); p.monitorWorkQueueGenerator(p.ctx) }()
		}

		// Wait for the next time we fetch the app name list.
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

func (p *ReconcilerPool) updateAppNameList(ctx context.Context) error {
	// Compile the wildcard expression as a regex so we can use it to match.
	// slog.Info("updateAppNameList: beginning update", slog.String("appNamePattern", p.AppName), slog.String("orgSlug", p.OrganizationSlug))
	re, err := regexp.Compile(FormatWildcardAsRegexp(p.AppName))
	if err != nil {
		return fmt.Errorf("compile wildcard as regexp: %w", err)
	}

	// Fetch and cache the organization ID.
	if p.orgID == "" {
		// slog.Info("updateAppNameList: fetching organization ID", slog.String("orgSlug", p.OrganizationSlug))
		org, err := p.flyClient.GetOrganizationBySlug(ctx, p.OrganizationSlug)
		if err != nil {
			slog.Error("updateAppNameList: failed to get organization by slug", slog.Any("err", err))
			return fmt.Errorf("get organization by slug: %w", err)
		}
		p.orgID = org.ID
		// slog.Info("updateAppNameList: organization ID fetched", slog.String("orgID", p.orgID))
	}

	// slog.Info("updateAppNameList: fetching apps for organization", slog.String("orgID", p.orgID))
	apps, err := p.flyClient.GetAppsForOrganization(ctx, p.orgID)
	if err != nil {
		slog.Error("updateAppNameList: failed to get apps for organization", slog.Any("err", err))
		return fmt.Errorf("get apps for organization: %w", err)
	}
	// slog.Info("updateAppNameList: fetched apps for organization", slog.Int("count", len(apps)))

	p.apps.Lock()
	defer p.apps.Unlock()

	m := make(map[string]appInfo)
	for i := range apps {
		name := apps[i].Name

		// slog.Info("updateAppNameList: checking app", slog.String("app", name))

		// Match against wildcard expression.
		if !re.MatchString(name) {
			continue
		}

		// Reuse client, if possible.
		if info, ok := p.apps.m[name]; ok {
			m[name] = info
			continue
		}

		// Otherwise build a new client with our constructor.
		// slog.Info("updateAppNameList: creating new flaps client for matched app", slog.String("app", name))
		client, err := p.NewFlapsClient(ctx, name)
		if err != nil {
			slog.Error("updateAppNameList: failed to build flaps client", slog.String("app", name), slog.Any("err", err))
			return fmt.Errorf("cannot build flaps client for app %q: %w", name, err)
		}
		m[name] = appInfo{
			name:   name,
			client: client,
		}
	}

	// Replace entire map so we
	p.apps.m = m
	// slog.Info("updateAppNameList: completed update", slog.Int("matched_app_count", len(m)))

	return nil
}

// monitorReconciler monitors the work queue and passes apps to the reconciler.
func (p *ReconcilerPool) monitorReconciler(ctx context.Context, r *Reconciler) {
	errReconciliationTimeout := fmt.Errorf("reconciliation timeout")

	for {
		select {
		case <-ctx.Done():
			return
		case info := <-p.ch:
			// Defend against panics within an individual app's reconciliation cycle.
			func() { // Anonymous function for one app processing cycle
				// General panic recovery for the entire app processing cycle (e.g., from CollectMetrics, Reconcile)
				defer func() {
					if rec := recover(); rec != nil {
						slog.ErrorContext(ctx, "monitorReconciler: recovered from UNHANDLED panic during app processing",
							slog.String("app", info.name),
							slog.Any("panic_info", rec),
						)
					}
				}()

				// slog.Info("monitorReconciler: received app from channel", slog.String("app", info.name))
				appCtx, cancel := context.WithTimeoutCause(p.ctx, p.ReconcileTimeout, errReconciliationTimeout)
				defer cancel()

				r.AppName = info.name
				r.Client = info.client

				// --- Logic for GetAppCurrentReleaseMachines with specific panic handling ---
				proceedToMetricsAndReconciliation := true // Assume we proceed, unless a condition below sets this to false.

				func() { // Inner anonymous function to isolate GetAppCurrentReleaseMachines call and its panic recovery
					defer func() {
						if rec := recover(); rec != nil {
							// slog.ErrorContext(appCtx, "monitorReconciler: ignoring panic during GetAppCurrentReleaseMachines call (this is a fly library error). Proceeding with metrics/reconciliation for app.",
							// 	slog.String("app", info.name),
							// 	slog.Any("panic_info", rec),
							// )
							// proceedToMetricsAndReconciliation remains true by default if a panic occurs here.
						}
					}()

					// slog.Info("monitorReconciler: calling GetAppCurrentReleaseMachines", slog.String("app", info.name))
					release, err := p.flyClient.GetAppCurrentReleaseMachines(appCtx, info.name)

					if err != nil { // Non-panic error from GetAppCurrentReleaseMachines
						slog.ErrorContext(appCtx, "GetAppCurrentReleaseMachines returned error. Skipping reconciliation for this app.",
							slog.String("app", info.name), slog.Any("err", err))
						proceedToMetricsAndReconciliation = false
						return
					}

					// Success path for GetAppCurrentReleaseMachines
					// slog.Info("monitorReconciler: GetAppCurrentReleaseMachines success", slog.String("app", info.name), slog.String("release_status", release.Status), slog.Bool("release_inprogress", release.InProgress))
					if release.Status == "running" {
						slog.WarnContext(appCtx, "Release is in progress (release.Status == \"running\"). Skipping reconciliation for this app.",
							slog.String("app", r.AppName))
						proceedToMetricsAndReconciliation = false
						return
					}
				}() // End of inner anonymous function for GetAppCurrentReleaseMachines
				// --- End of logic for GetAppCurrentReleaseMachines ---

				if !proceedToMetricsAndReconciliation {
					// slog.Info("monitorReconciler: Bypassing metrics and reconciliation for app due to release check outcome.", slog.String("app", info.name))
					return // Exit the main anonymous function for this app.
				}

				// slog.Info("monitorReconciler: proceeding to CollectMetrics", slog.String("app", info.name))
				if err := r.CollectMetrics(appCtx); err != nil {
					slog.ErrorContext(appCtx, "metrics collection failed",
						slog.String("app", info.name),
						slog.Any("err", err))
					return 
				}
				// slog.Info("monitorReconciler: CollectMetrics success", slog.String("app", info.name))

				// slog.Info("monitorReconciler: calling Reconcile", slog.String("app", info.name))
				if err := r.Reconcile(appCtx); err != nil {
					slog.ErrorContext(appCtx, "reconciliation failed",
						slog.String("app", info.name),
						slog.Any("err", err))
					return 
				}
				// slog.Info("monitorReconciler: Reconcile success", slog.String("app", info.name))
			}() // End of the main anonymous function for app processing.
		}
	}
}

func (p *ReconcilerPool) RegisterPromMetrics(reg prometheus.Registerer) {
	p.registerMachineStartCount(reg)
	p.registerMachineStoppedCount(reg)
	p.registerReconcileCount(reg)
}

func (p *ReconcilerPool) registerMachineStartCount(reg prometheus.Registerer) {
	const name = "fas_machine_start_count"

	reg.MustRegister(prometheus.NewCounterFunc(
		prometheus.CounterOpts{
			Name:        name,
			ConstLabels: prometheus.Labels{"status": "ok"},
		},
		func() float64 { return float64(p.Stats.MachineStarted.Load()) },
	))
	reg.MustRegister(prometheus.NewCounterFunc(
		prometheus.CounterOpts{
			Name:        name,
			ConstLabels: prometheus.Labels{"status": "failed"},
		},
		func() float64 { return float64(p.Stats.MachineStartFailed.Load()) },
	))
}

func (p *ReconcilerPool) registerMachineStoppedCount(reg prometheus.Registerer) {
	const name = "fas_machine_stop_count"

	reg.MustRegister(prometheus.NewCounterFunc(
		prometheus.CounterOpts{
			Name:        name,
			ConstLabels: prometheus.Labels{"status": "ok"},
		},
		func() float64 { return float64(p.Stats.MachineStopped.Load()) },
	))
	reg.MustRegister(prometheus.NewCounterFunc(
		prometheus.CounterOpts{
			Name:        name,
			ConstLabels: prometheus.Labels{"status": "failed"},
		},
		func() float64 { return float64(p.Stats.MachineStopFailed.Load()) },
	))
}

func (p *ReconcilerPool) registerReconcileCount(reg prometheus.Registerer) {
	const name = "fas_reconcile_count"

	reg.MustRegister(prometheus.NewCounterFunc(
		prometheus.CounterOpts{
			Name:        name,
			ConstLabels: prometheus.Labels{"status": "create"},
		},
		func() float64 { return float64(p.Stats.BulkCreate.Load()) },
	))

	reg.MustRegister(prometheus.NewCounterFunc(
		prometheus.CounterOpts{
			Name:        name,
			ConstLabels: prometheus.Labels{"status": "destroy"},
		},
		func() float64 { return float64(p.Stats.BulkDestroy.Load()) },
	))

	reg.MustRegister(prometheus.NewCounterFunc(
		prometheus.CounterOpts{
			Name:        name,
			ConstLabels: prometheus.Labels{"status": "start"},
		},
		func() float64 { return float64(p.Stats.BulkStart.Load()) },
	))

	reg.MustRegister(prometheus.NewCounterFunc(
		prometheus.CounterOpts{
			Name:        name,
			ConstLabels: prometheus.Labels{"status": "stop"},
		},
		func() float64 { return float64(p.Stats.BulkStop.Load()) },
	))

	reg.MustRegister(prometheus.NewCounterFunc(
		prometheus.CounterOpts{
			Name:        name,
			ConstLabels: prometheus.Labels{"status": "no_scale"},
		},
		func() float64 { return float64(p.Stats.NoScale.Load()) },
	))
}

type appInfo struct {
	name   string
	client FlapsClient
}

// FormatWildcardAsRegexp returns a regexp for a given wildcard expression.
func FormatWildcardAsRegexp(s string) string {
	if s == "" {
		return ".*"
	}

	a := strings.Split(s, "*")
	for i := range a {
		a[i] = regexp.QuoteMeta(a[i])
	}
	return "^" + strings.Join(a, ".*") + "$"
}

var errReconcilerPoolClosing = errors.New("reconciler pool closing")
