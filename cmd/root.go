package cmd

import (
	"errors"
	"math"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/grioghar/lighthouse/internal/actions"
	"github.com/grioghar/lighthouse/internal/flags"
	"github.com/grioghar/lighthouse/internal/meta"
	"github.com/grioghar/lighthouse/pkg/api"
	apiMetrics "github.com/grioghar/lighthouse/pkg/api/metrics"
	"github.com/grioghar/lighthouse/pkg/api/rest"
	"github.com/grioghar/lighthouse/pkg/api/store"
	"github.com/grioghar/lighthouse/pkg/api/update"
	"github.com/grioghar/lighthouse/pkg/config"
	"github.com/grioghar/lighthouse/pkg/container"
	"github.com/grioghar/lighthouse/pkg/filters"
	"github.com/grioghar/lighthouse/pkg/metrics"
	"github.com/grioghar/lighthouse/pkg/notifications"
	t "github.com/grioghar/lighthouse/pkg/types"
	"github.com/robfig/cron"
	log "github.com/sirupsen/logrus"

	"github.com/spf13/cobra"
)

var (
	client            container.Client
	scheduleSpec      string
	cleanup           bool
	noRestart         bool
	noPull            bool
	monitorOnly       bool
	enableLabel       bool
	disableContainers []string
	notifier          t.Notifier
	timeout           time.Duration
	lifecycleHooks    bool
	rollingRestart    bool
	scope             string
	labelPrecedence   bool
	healthGated       bool
	healthTimeout     time.Duration
	sessionStore      *store.Store
	settingsStore     *config.Store
)

var rootCmd = NewRootCommand()

// NewRootCommand creates the root command for lighthouse
func NewRootCommand() *cobra.Command {
	return &cobra.Command{
		Use:   "lighthouse",
		Short: "Automatically updates running Docker containers",
		Long: `
	Lighthouse automatically updates running Docker containers whenever a new image is released.
	Lighthouse is a maintained fork of containrrr/watchtower.
	More information available at https://github.com/grioghar/lighthouse/.
	`,
		Run:    Run,
		PreRun: PreRun,
		Args:   cobra.ArbitraryArgs,
	}
}

func init() {
	flags.SetDefaults()
	flags.RegisterDockerFlags(rootCmd)
	flags.RegisterSystemFlags(rootCmd)
	flags.RegisterNotificationFlags(rootCmd)
}

// Execute the root func and exit in case of errors
func Execute() {
	rootCmd.AddCommand(notifyUpgradeCommand)
	if err := rootCmd.Execute(); err != nil {
		log.Fatal(err)
	}
}

// PreRun is a lifecycle hook that runs before the command is executed.
func PreRun(cmd *cobra.Command, _ []string) {
	f := cmd.PersistentFlags()
	flags.ProcessFlagAliases(f)
	if err := flags.SetupLogging(f); err != nil {
		log.Fatalf("Failed to initialize logging: %s", err.Error())
	}

	scheduleSpec, _ = f.GetString("schedule")

	flags.GetSecretsFromFiles(cmd)
	cleanup, noRestart, monitorOnly, timeout = flags.ReadFlags(cmd)

	if timeout < 0 {
		log.Fatal("Please specify a positive value for timeout value.")
	}

	enableLabel, _ = f.GetBool("label-enable")
	disableContainers, _ = f.GetStringSlice("disable-containers")
	lifecycleHooks, _ = f.GetBool("enable-lifecycle-hooks")
	rollingRestart, _ = f.GetBool("rolling-restart")
	scope, _ = f.GetString("scope")
	labelPrecedence, _ = f.GetBool("label-take-precedence")
	healthGated, _ = f.GetBool("health-gated")
	healthTimeout, _ = f.GetDuration("health-timeout")

	if scope != "" {
		log.Debugf(`Using scope %q`, scope)
	}

	// configure environment vars for client
	err := flags.EnvConfig(cmd)
	if err != nil {
		log.Fatal(err)
	}

	noPull, _ = f.GetBool("no-pull")
	includeStopped, _ := f.GetBool("include-stopped")
	includeRestarting, _ := f.GetBool("include-restarting")
	reviveStopped, _ := f.GetBool("revive-stopped")
	removeVolumes, _ := f.GetBool("remove-volumes")
	warnOnHeadPullFailed, _ := f.GetString("warn-on-head-failure")

	if monitorOnly && noPull {
		log.Warn("Using `WATCHTOWER_NO_PULL` and `WATCHTOWER_MONITOR_ONLY` simultaneously might lead to no action being taken at all. If this is intentional, you may safely ignore this message.")
	}

	client = container.NewClient(container.ClientOptions{
		IncludeStopped:    includeStopped,
		ReviveStopped:     reviveStopped,
		RemoveVolumes:     removeVolumes,
		IncludeRestarting: includeRestarting,
		WarnOnHeadFailed:  container.WarningStrategy(warnOnHeadPullFailed),
	})

	notifier = notifications.NewNotifier(cmd)
	notifier.AddLogHook()
}

// Run is the main execution flow of the command
func Run(c *cobra.Command, names []string) {
	filter, filterDesc := filters.BuildFilter(names, disableContainers, enableLabel, scope)
	runOnce, _ := c.PersistentFlags().GetBool("run-once")
	enableUpdateAPI, _ := c.PersistentFlags().GetBool("http-api-update")
	enableMetricsAPI, _ := c.PersistentFlags().GetBool("http-api-metrics")
	unblockHTTPAPI, _ := c.PersistentFlags().GetBool("http-api-periodic-polls")
	apiToken, _ := c.PersistentFlags().GetString("http-api-token")
	healthCheck, _ := c.PersistentFlags().GetBool("health-check")

	if healthCheck {
		// health check should not have pid 1
		if os.Getpid() == 1 {
			time.Sleep(1 * time.Second)
			log.Fatal("The health check flag should never be passed to the main lighthouse container process")
		}
		os.Exit(0)
	}

	if rollingRestart && monitorOnly {
		log.Fatal("Rolling restarts is not compatible with the global monitor only flag")
	}

	awaitDockerClient()

	// Records recent scan sessions for the web UI / history API.
	sessionStore = store.New(50)

	// Runtime-adjustable settings (editable via the web console), seeded from the
	// startup flags and optionally persisted to --config-file.
	configFile, _ := c.PersistentFlags().GetString("config-file")
	var cfgErr error
	settingsStore, cfgErr = config.NewStore(configFile, config.Settings{
		Cleanup:              cleanup,
		NoRestart:            noRestart,
		MonitorOnly:          monitorOnly,
		NoPull:               noPull,
		LifecycleHooks:       lifecycleHooks,
		RollingRestart:       rollingRestart,
		HealthGated:          healthGated,
		HealthTimeoutSeconds: int(healthTimeout.Seconds()),
	})
	if cfgErr != nil {
		log.Fatalf("Failed to load config file %q: %v", configFile, cfgErr)
	}

	if err := actions.CheckForSanity(client, filter, rollingRestart); err != nil {
		logNotifyExit(err)
	}

	if runOnce {
		writeStartupMessage(c, time.Time{}, filterDesc)
		runUpdatesWithNotifications(filter, store.TriggerStartup)
		notifier.Close()
		os.Exit(0)
		return
	}

	if err := actions.CheckForMultipleWatchtowerInstances(client, cleanup, scope); err != nil {
		logNotifyExit(err)
	}

	// The lock is shared between the scheduler and the HTTP API. It only allows one update to run at a time.
	updateLock := make(chan bool, 1)
	updateLock <- true

	enableWeb, _ := c.PersistentFlags().GetBool("web")
	webAddress, _ := c.PersistentFlags().GetString("web-address")
	sessionSecret, _ := c.PersistentFlags().GetString("session-secret")

	httpAPI := api.New(apiToken)
	if webAddress != "" {
		httpAPI.Address = webAddress
	}
	httpAPI.TLSCert, _ = c.PersistentFlags().GetString("tls-cert")
	httpAPI.TLSKey, _ = c.PersistentFlags().GetString("tls-key")

	// triggerUpdate runs an update for the given images (empty = all watched
	// containers). Shared by the legacy /v1/update endpoint and the web UI's
	// scan actions; the caller is responsible for holding the update lock.
	triggerUpdate := func(images []string) {
		metric := runUpdatesWithNotifications(filters.FilterByImage(images, filter), store.TriggerAPI)
		metrics.RegisterScan(metric)
	}

	if enableUpdateAPI {
		updateHandler := update.New(triggerUpdate, updateLock)
		httpAPI.RegisterFunc(updateHandler.Path, updateHandler.Handle)
		// If polling isn't enabled the scheduler is never started, and
		// we need to trigger the startup messages manually.
		if !unblockHTTPAPI && !enableWeb {
			writeStartupMessage(c, time.Time{}, filterDesc)
		}
	}

	if enableMetricsAPI {
		metricsHandler := apiMetrics.New()
		httpAPI.RegisterHandler(metricsHandler.Path, metricsHandler.Handle)
	}

	if enableWeb {
		hub := rest.NewHub()
		log.AddHook(hub)
		if err := httpAPI.EnableWeb(rest.Deps{
			Version:  meta.Version,
			Client:   client,
			Store:    sessionStore,
			Filter:   filter,
			Trigger:  triggerUpdate,
			Lock:     updateLock,
			Config:   buildConfigInfo(filterDesc),
			Settings: settingsStore,
			Hub:      hub,
		}, sessionSecret, httpAPI.TLSEnabled()); err != nil {
			log.Errorf("Failed to enable web UI: %v", err)
		}
	}

	// Block on the HTTP server only when it is the sole long-running task
	// (update API enabled, no periodic polls, and no web UI keeping the
	// scheduler alive).
	blockOnAPI := enableUpdateAPI && !unblockHTTPAPI && !enableWeb
	if err := httpAPI.Start(blockOnAPI); err != nil && !errors.Is(err, http.ErrServerClosed) {
		log.Error("failed to start API", err)
	}

	if err := runUpgradesOnSchedule(c, filter, filterDesc, updateLock); err != nil {
		log.Error(err)
	}

	os.Exit(1)
}

func logNotifyExit(err error) {
	log.Error(err)
	notifier.Close()
	os.Exit(1)
}

func awaitDockerClient() {
	log.Debug("Sleeping for a second to ensure the docker api client has been properly initialized.")
	time.Sleep(1 * time.Second)
}

func formatDuration(d time.Duration) string {
	sb := strings.Builder{}

	hours := int64(d.Hours())
	minutes := int64(math.Mod(d.Minutes(), 60))
	seconds := int64(math.Mod(d.Seconds(), 60))

	if hours == 1 {
		sb.WriteString("1 hour")
	} else if hours != 0 {
		sb.WriteString(strconv.FormatInt(hours, 10))
		sb.WriteString(" hours")
	}

	if hours != 0 && (seconds != 0 || minutes != 0) {
		sb.WriteString(", ")
	}

	if minutes == 1 {
		sb.WriteString("1 minute")
	} else if minutes != 0 {
		sb.WriteString(strconv.FormatInt(minutes, 10))
		sb.WriteString(" minutes")
	}

	if minutes != 0 && (seconds != 0) {
		sb.WriteString(", ")
	}

	if seconds == 1 {
		sb.WriteString("1 second")
	} else if seconds != 0 || (hours == 0 && minutes == 0) {
		sb.WriteString(strconv.FormatInt(seconds, 10))
		sb.WriteString(" seconds")
	}

	return sb.String()
}

func writeStartupMessage(c *cobra.Command, sched time.Time, filtering string) {
	noStartupMessage, _ := c.PersistentFlags().GetBool("no-startup-message")
	enableUpdateAPI, _ := c.PersistentFlags().GetBool("http-api-update")

	var startupLog *log.Entry
	if noStartupMessage {
		startupLog = notifications.LocalLog
	} else {
		startupLog = log.NewEntry(log.StandardLogger())
		// Batch up startup messages to send them as a single notification
		notifier.StartNotification()
	}

	startupLog.Info("Lighthouse ", meta.Version)

	notifierNames := notifier.GetNames()
	if len(notifierNames) > 0 {
		startupLog.Info("Using notifications: " + strings.Join(notifierNames, ", "))
	} else {
		startupLog.Info("Using no notifications")
	}

	startupLog.Info(filtering)

	if !sched.IsZero() {
		until := formatDuration(time.Until(sched))
		startupLog.Info("Scheduling first run: " + sched.Format("2006-01-02 15:04:05 -0700 MST"))
		startupLog.Info("Note that the first check will be performed in " + until)
	} else if runOnce, _ := c.PersistentFlags().GetBool("run-once"); runOnce {
		startupLog.Info("Running a one time update.")
	} else {
		startupLog.Info("Periodic runs are not enabled.")
	}

	if enableUpdateAPI {
		// TODO: make listen port configurable
		startupLog.Info("The HTTP API is enabled at :8080.")
	}

	if !noStartupMessage {
		// Send the queued up startup messages, not including the trace warning below (to make sure it's noticed)
		notifier.SendNotification(nil)
	}

	if log.IsLevelEnabled(log.TraceLevel) {
		startupLog.Warn("Trace level enabled: log will include sensitive information as credentials and tokens")
	}
}

func runUpgradesOnSchedule(c *cobra.Command, filter t.Filter, filtering string, lock chan bool) error {
	if lock == nil {
		lock = make(chan bool, 1)
		lock <- true
	}

	scheduler := cron.New()
	err := scheduler.AddFunc(
		scheduleSpec,
		func() {
			select {
			case v := <-lock:
				defer func() { lock <- v }()
				metric := runUpdatesWithNotifications(filter, store.TriggerSchedule)
				metrics.RegisterScan(metric)
			default:
				// Update was skipped
				metrics.RegisterScan(nil)
				log.Debug("Skipped another update already running.")
			}

			nextRuns := scheduler.Entries()
			if len(nextRuns) > 0 {
				log.Debug("Scheduled next run: " + nextRuns[0].Next.String())
			}
		})

	if err != nil {
		return err
	}

	writeStartupMessage(c, scheduler.Entries()[0].Schedule.Next(time.Now()), filtering)

	scheduler.Start()

	// Graceful shut-down on SIGINT/SIGTERM
	interrupt := make(chan os.Signal, 1)
	signal.Notify(interrupt, os.Interrupt)
	signal.Notify(interrupt, syscall.SIGTERM)

	<-interrupt
	scheduler.Stop()
	log.Info("Waiting for running update to be finished...")
	<-lock
	return nil
}

func runUpdatesWithNotifications(filter t.Filter, trigger string) *metrics.Metric {
	notifier.StartNotification()
	// Behavior toggles come from the runtime settings store (web-console editable),
	// falling back to the startup flag values if the store isn't initialized.
	s := config.Settings{
		Cleanup:              cleanup,
		NoRestart:            noRestart,
		MonitorOnly:          monitorOnly,
		NoPull:               noPull,
		LifecycleHooks:       lifecycleHooks,
		RollingRestart:       rollingRestart,
		HealthGated:          healthGated,
		HealthTimeoutSeconds: int(healthTimeout.Seconds()),
	}
	if settingsStore != nil {
		s = settingsStore.Get()
	}
	updateParams := t.UpdateParams{
		Filter:          filter,
		Cleanup:         s.Cleanup,
		NoRestart:       s.NoRestart,
		Timeout:         timeout,
		MonitorOnly:     s.MonitorOnly,
		LifecycleHooks:  s.LifecycleHooks,
		RollingRestart:  s.RollingRestart,
		LabelPrecedence: labelPrecedence,
		NoPull:          s.NoPull,
		HealthGated:     s.HealthGated,
		HealthTimeout:   time.Duration(s.HealthTimeoutSeconds) * time.Second,
	}
	start := time.Now()
	result, err := actions.Update(client, updateParams)
	if err != nil {
		log.Error(err)
	}
	if sessionStore != nil {
		sessionStore.Record(result, start, time.Since(start), trigger)
	}
	notifier.SendNotification(result)
	metricResults := metrics.NewMetric(result)
	notifications.LocalLog.WithFields(log.Fields{
		"Scanned": metricResults.Scanned,
		"Updated": metricResults.Updated,
		"Failed":  metricResults.Failed,
	}).Info("Session done")
	return metricResults
}

// buildConfigInfo returns a redacted, read-only snapshot of the running
// configuration for the web UI / config API. It intentionally contains no
// secrets (API token, notification URLs/passwords).
func buildConfigInfo(filtering string) rest.ConfigInfo {
	return rest.ConfigInfo{
		Schedule:       scheduleSpec,
		Filtering:      filtering,
		Cleanup:        cleanup,
		NoRestart:      noRestart,
		NoPull:         noPull,
		MonitorOnly:    monitorOnly,
		LabelEnable:    enableLabel,
		RollingRestart: rollingRestart,
		LifecycleHooks: lifecycleHooks,
		HealthGated:    healthGated,
		HealthTimeout:  healthTimeout.String(),
		Scope:          scope,
		Notifiers:      notifier.GetNames(),
	}
}
