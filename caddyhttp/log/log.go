// Package log implements request (access) logging middleware.
package log

import (
	"fmt"
	"log"
	"net/http"
	"os"

	"sync"

	"github.com/mholt/caddy"
	"github.com/mholt/caddy/caddyhttp/httpserver"
)

func init() {
	caddy.RegisterPlugin("log", caddy.Plugin{
		ServerType: "http",
		Action:     setup,
	})
}

// Logger is a basic request logging middleware.
type Logger struct {
	Next      httpserver.Handler
	Rules     []*Rule
	ErrorFunc func(http.ResponseWriter, *http.Request, int) // failover error handler
}

func (l Logger) ServeHTTP(w http.ResponseWriter, r *http.Request) (int, error) {
	for _, rule := range l.Rules {
		if httpserver.Path(r.URL.Path).Matches(rule.PathScope) {
			// Record the response
			responseRecorder := httpserver.NewResponseRecorder(w)

			// Attach the Replacer we'll use so that other middlewares can
			// set their own placeholders if they want to.
			rep := httpserver.NewReplacer(r, responseRecorder, CommonLogEmptyValue)
			responseRecorder.Replacer = rep

			// Bon voyage, request!
			status, err := l.Next.ServeHTTP(responseRecorder, r)

			if status >= 400 {
				// There was an error up the chain, but no response has been written yet.
				// The error must be handled here so the log entry will record the response size.
				if l.ErrorFunc != nil {
					l.ErrorFunc(responseRecorder, r, status)
				} else {
					// Default failover error handler
					responseRecorder.WriteHeader(status)
					fmt.Fprintf(responseRecorder, "%d %s", status, http.StatusText(status))
				}
				status = 0
			}

			// Write log entries
			for _, e := range rule.Entries {

				// Store logfile as per caddyfile to revert.
				entryOutputFile := e.OutputFile

				// See if we need to replace anypart of the logfile name
				// if different from current open file then close and reopen
				logfn := rep.Replace(e.OutputFile)

				if logfn != e.OutputFile && logfn != e.file.Name() {

					// Close current Logfile
					e.file.Close()
					e.file.Name()

					// TODO: not sure if i need to do something more with log before setting to nil
					e.Log = nil

					e.OutputFile = logfn
					err := OpenLogFile(e)
					if err != nil {
						return status, err
					}
					// Reset back to outputfile as per caddyfile
					e.OutputFile = entryOutputFile
				}

				e.fileMu.RLock()
				e.Log.Println(rep.Replace(e.Format))
				e.fileMu.RUnlock()
			}

			return status, err
		}
	}
	return l.Next.ServeHTTP(w, r)
}

// Entry represents a log entry under a path scope
type Entry struct {
	OutputFile string
	Format     string
	Log        *log.Logger
	Roller     *httpserver.LogRoller
	file       *os.File      // if logging to a file that needs to be closed
	fileMu     *sync.RWMutex // files can't be safely read/written in one goroutine and closed in another (issue #1371)
}

// Rule configures the logging middleware.
type Rule struct {
	PathScope string
	Entries   []*Entry
}

const (
	// DefaultLogFilename is the default log filename.
	DefaultLogFilename = "access.log"
	// CommonLogFormat is the common log format.
	CommonLogFormat = `{remote} ` + CommonLogEmptyValue + " " + CommonLogEmptyValue + ` [{when}] "{method} {uri} {proto}" {status} {size}`
	// CommonLogEmptyValue is the common empty log value.
	CommonLogEmptyValue = "-"
	// CombinedLogFormat is the combined log format.
	CombinedLogFormat = CommonLogFormat + ` "{>Referer}" "{>User-Agent}"`
	// DefaultLogFormat is the default log format.
	DefaultLogFormat = CommonLogFormat
)
