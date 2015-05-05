// app - pstop application package
//
// This file contains the library routines related to running the app.
package app

import (
	"database/sql"
	"errors"
	"fmt"
	"log"
	"os"
	"os/signal"
	"regexp"
	"strings"
	"syscall"
	"time"

	"github.com/sjmudd/pstop/display"
	"github.com/sjmudd/pstop/event"
	"github.com/sjmudd/pstop/i_s/processlist"
	"github.com/sjmudd/pstop/lib"
	essgben "github.com/sjmudd/pstop/p_s/events_stages_summary_global_by_event_name"
	ewsgben "github.com/sjmudd/pstop/p_s/events_waits_summary_global_by_event_name"
	fsbi "github.com/sjmudd/pstop/p_s/file_summary_by_instance"
	"github.com/sjmudd/pstop/p_s/ps_table"
	"github.com/sjmudd/pstop/p_s/setup_instruments"
	tiwsbt "github.com/sjmudd/pstop/p_s/table_io_waits_summary_by_table"
	tlwsbt "github.com/sjmudd/pstop/p_s/table_lock_waits_summary_by_table"
	"github.com/sjmudd/pstop/screen"
	"github.com/sjmudd/pstop/version"
	"github.com/sjmudd/pstop/view"
	"github.com/sjmudd/pstop/wait_info"
)

var (
	re_valid_version = regexp.MustCompile(`^(5\.[67]\.|10\.[01])`)
)

type App struct {
	count               int
	display             display.Display
	done                chan struct{}
	sigChan             chan os.Signal
	wi                  wait_info.WaitInfo
	finished            bool
	limit               int
	stdout              bool
	dbh                 *sql.DB
	help                bool
	hostname            string
	fsbi                ps_table.Tabler // ufsbi.File_summary_by_instance
	tiwsbt              tiwsbt.Object
	tlwsbt              ps_table.Tabler // tlwsbt.Table_lock_waits_summary_by_table
	ewsgben             ps_table.Tabler // ewsgben.Events_waits_summary_global_by_event_name
	essgben             ps_table.Tabler // essgben.Events_stages_summary_global_by_event_name
	users               processlist.Object
	screen              screen.TermboxScreen
	view                view.View
	mysql_version       string
	want_relative_stats bool
	wait_info.WaitInfo  // embedded
	setup_instruments   setup_instruments.SetupInstruments
}

func (app *App) Setup(dbh *sql.DB, interval int, count int, stdout bool, limit int, default_view string) {
	lib.Logger.Println("app.Setup()")

	app.count = count
	app.dbh = dbh
	app.finished = false
	app.limit = limit
	app.stdout = stdout

	if stdout {
		app.display = new(display.StdoutDisplay)
	} else {
		app.display = new(display.ScreenDisplay)
	}
	app.display.Setup()
	app.SetHelp(false)
	app.view.SetByName(default_view) // if empty will use the default

	if err := app.validate_mysql_version(); err != nil {
		log.Fatal(err)
	}

	app.setup_instruments = setup_instruments.NewSetupInstruments(dbh)
	app.setup_instruments.EnableMonitoring()

	app.wi.SetWaitInterval(time.Second * time.Duration(interval))

	_, variables := lib.SelectAllGlobalVariablesByVariableName(app.dbh)
	// setup to their initial types/values
	app.fsbi = fsbi.NewFileSummaryByInstance(variables)
	app.tlwsbt = new(tlwsbt.Object)
	app.ewsgben = new(ewsgben.Object)
	app.essgben = new(essgben.Object)

	app.want_relative_stats = true // we show info from the point we start collecting data
	app.fsbi.SetWantRelativeStats(app.want_relative_stats)
	app.fsbi.SetNow()
	app.tlwsbt.SetWantRelativeStats(app.want_relative_stats)
	app.tlwsbt.SetNow()
	app.tiwsbt.SetWantRelativeStats(app.want_relative_stats)
	app.tiwsbt.SetNow()
	app.users.SetWantRelativeStats(app.want_relative_stats) // ignored
	app.users.SetNow()                                      // ignored
	app.essgben.SetWantRelativeStats(app.want_relative_stats)
	app.essgben.SetNow()
	app.ewsgben.SetWantRelativeStats(app.want_relative_stats) // ignored
	app.ewsgben.SetNow()                                      // ignored

	app.ResetDBStatistics()

	app.tiwsbt.SetWantsLatency(true)

	// get short name (to save space)
	_, hostname := lib.SelectGlobalVariableByVariableName(app.dbh, "HOSTNAME")
	if index := strings.Index(hostname, "."); index >= 0 {
		hostname = hostname[0:index]
	}
	_, mysql_version := lib.SelectGlobalVariableByVariableName(app.dbh, "VERSION")

	// setup display with base data
	app.display.SetHostname(hostname)
	app.display.SetMySQLVersion(mysql_version)
	app.display.SetVersion(version.Version())
	app.display.SetMyname(lib.MyName())
	app.display.SetWantRelativeStats(app.want_relative_stats)
}

// have we finished ?
func (app App) Finished() bool {
	return app.finished
}

// do a fresh collection of data and then update the initial values based on that.
func (app *App) ResetDBStatistics() {
	app.fsbi.Collect(app.dbh)
	app.tlwsbt.Collect(app.dbh)
	app.tiwsbt.Collect(app.dbh)
	app.essgben.Collect(app.dbh)
	app.ewsgben.Collect(app.dbh)
	app.SyncReferenceValues()
}

func (app *App) SyncReferenceValues() {
	start := time.Now()
	app.fsbi.SyncReferenceValues()
	app.tlwsbt.SyncReferenceValues()
	app.tiwsbt.SyncReferenceValues()
	app.essgben.SyncReferenceValues()
	app.ewsgben.SyncReferenceValues()
	app.updateLast()
	lib.Logger.Println("app.SyncReferenceValues() took", time.Duration(time.Since(start)).String())
}

// update the last time that have relative data for
func (app *App) updateLast() {
	switch app.view.Get() {
	case view.ViewLatency, view.ViewOps:
		app.display.SetLast(app.tiwsbt.Last())
	case view.ViewIO:
		app.display.SetLast(app.fsbi.Last())
	case view.ViewLocks:
		app.display.SetLast(app.tlwsbt.Last())
	case view.ViewUsers:
		app.display.SetLast(app.users.Last())
	case view.ViewMutex:
		app.display.SetLast(app.ewsgben.Last())
	case view.ViewStages:
		app.display.SetLast(app.essgben.Last())
	}
}

// Only collect the data we are looking at.
func (app *App) Collect() {
	start := time.Now()

	switch app.view.Get() {
	case view.ViewLatency, view.ViewOps:
		app.tiwsbt.Collect(app.dbh)
	case view.ViewIO:
		app.fsbi.Collect(app.dbh)
	case view.ViewLocks:
		app.tlwsbt.Collect(app.dbh)
	case view.ViewUsers:
		app.users.Collect(app.dbh)
	case view.ViewMutex:
		app.ewsgben.Collect(app.dbh)
	case view.ViewStages:
		app.essgben.Collect(app.dbh)
	}
	app.updateLast()
	app.wi.CollectedNow()
	lib.Logger.Println("app.Collect() took", time.Duration(time.Since(start)).String())
}

func (app *App) SetHelp(newHelp bool) {
	app.help = newHelp

	app.display.ClearAndFlush()
}

func (app *App) SetMySQLVersion(mysql_version string) {
	app.mysql_version = mysql_version
}

func (app *App) SetHostname(hostname string) {
	lib.Logger.Println("app.SetHostname(", hostname, ")")
	app.hostname = hostname
}

func (app App) Help() bool {
	return app.help
}

// display the output according to the mode we are in
func (app *App) Display() {
	if app.help {
		app.display.DisplayHelp() // shouldn't get here if in --stdout mode
	} else {
		_, uptime := lib.SelectGlobalStatusByVariableName(app.dbh, "UPTIME")
		app.display.SetUptime(uptime)

		switch app.view.Get() {
		case view.ViewLatency, view.ViewOps:
			app.display.DisplayOpsOrLatency(app.tiwsbt)
		case view.ViewIO:
			app.display.DisplayIO(app.fsbi)
		case view.ViewLocks:
			app.display.DisplayLocks(app.tlwsbt)
		case view.ViewUsers:
			app.display.DisplayUsers(app.users)
		case view.ViewMutex:
			app.display.DisplayMutex(app.ewsgben)
		case view.ViewStages:
			app.display.DisplayStages(app.essgben)
		}
	}
}

// fix_latency_setting() ensures the SetWantsLatency() value is
// correct. This needs to be done more cleanly.
func (app *App) fix_latency_setting() {
	if app.view.Get() == view.ViewLatency {
		app.tiwsbt.SetWantsLatency(true)
	}
	if app.view.Get() == view.ViewOps {
		app.tiwsbt.SetWantsLatency(false)
	}
}

// change to the previous display mode
func (app *App) DisplayPrevious() {
	app.view.SetPrev()
	app.fix_latency_setting()
	app.display.ClearAndFlush()
}

// change to the next display mode
func (app *App) DisplayNext() {
	app.view.SetNext()
	app.fix_latency_setting()
	app.display.ClearAndFlush()
}

// do we want to show all p_s data?
func (app App) WantRelativeStats() bool {
	return app.want_relative_stats
}

// set if we want data from when we started/reset stats.
func (app *App) SetWantRelativeStats(want_relative_stats bool) {
	app.want_relative_stats = want_relative_stats

	app.fsbi.SetWantRelativeStats(want_relative_stats)
	app.tlwsbt.SetWantRelativeStats(app.want_relative_stats)
	app.tiwsbt.SetWantRelativeStats(app.want_relative_stats)
	app.ewsgben.SetWantRelativeStats(app.want_relative_stats)
	app.essgben.SetWantRelativeStats(app.want_relative_stats)
	app.display.SetWantRelativeStats(app.want_relative_stats)
}

// clean up screen and disconnect database
func (app *App) Cleanup() {
	app.display.Close()
	if app.dbh != nil {
		app.setup_instruments.RestoreConfiguration()
		_ = app.dbh.Close()
	}
}

// get into a run loop
func (app *App) Run() {
	lib.Logger.Println("app.Run()")

	app.sigChan = make(chan os.Signal, 10) // 10 entries
	signal.Notify(app.sigChan, syscall.SIGINT, syscall.SIGTERM)

	eventChan := app.display.EventChan()

	for !app.Finished() {
		select {
		case sig := <-app.sigChan:
			fmt.Println("Caught signal: ", sig)
			app.finished = true
		case <-app.wi.WaitNextPeriod():
			app.Collect()
			app.Display()
		case input_event := <-eventChan:
			switch input_event.Type {
			case event.EventFinished:
				app.finished = true
			case event.EventViewNext:
				app.DisplayNext()
				app.Display()
			case event.EventViewPrev:
				app.DisplayPrevious()
				app.Display()
			case event.EventDecreasePollTime:
				if app.wi.WaitInterval() > time.Second {
					app.wi.SetWaitInterval(app.wi.WaitInterval() - time.Second)
				}
			case event.EventIncreasePollTime:
				app.wi.SetWaitInterval(app.wi.WaitInterval() + time.Second)
			case event.EventHelp:
				app.SetHelp(!app.Help())
			case event.EventToggleWantRelative:
				app.SetWantRelativeStats(!app.WantRelativeStats())
				app.Display()
			case event.EventResetStatistics:
				app.ResetDBStatistics()
				app.Display()
			case event.EventResizeScreen:
				width, height := input_event.Width, input_event.Height
				app.display.Resize(width, height)
				app.Display()
			case event.EventError:
				log.Fatalf("Quitting because of EventError error")
			}
		}
		// provide a hook to stop the application if the counter goes down to zero
		if app.stdout && app.count > 0 {
			app.count--
			if app.count == 0 {
				app.finished = true
			}
		}
	}
}

// pstop requires MySQL 5.6+ or MariaDB 10.0+. Check the version
// rather than giving an error message if the requires P_S tables can't
// be found.
func (app *App) validate_mysql_version() error {
	var tables = [...]string{
		"performance_schema.events_stages_summary_global_by_event_name",
		"performance_schema.events_waits_summary_global_by_event_name",
		"performance_schema.file_summary_by_instance",
		"performance_schema.table_io_waits_summary_by_table",
		"performance_schema.table_lock_waits_summary_by_table",
	}

	lib.Logger.Println("validate_mysql_version()")

	lib.Logger.Println("- Getting MySQL version")
	err, mysql_version := lib.SelectGlobalVariableByVariableName(app.dbh, "VERSION")
	if err != nil {
		return err
	}
	lib.Logger.Println("- mysql_version: '" + mysql_version + "'")

	if !re_valid_version.MatchString(mysql_version) {
		return errors.New(lib.MyName() + " does not work with MySQL version " + mysql_version)
	}
	lib.Logger.Println("OK: MySQL version is valid, continuing")

	lib.Logger.Println("Checking access to required tables:")
	for i := range tables {
		if err := lib.CheckTableAccess(app.dbh, tables[i]); err == nil {
			lib.Logger.Println("OK: " + tables[i] + " found")
		} else {
			return err
		}
	}
	lib.Logger.Println("OK: all table checks passed")

	return nil
}
