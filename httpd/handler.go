package httpd

import (
	"compress/gzip"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/ioutil"
	"log"
	"net/http"
	"net/http/pprof"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/bmizerany/pat"
	"github.com/influxdb/influxdb"
	"github.com/influxdb/influxdb/client"
	"github.com/influxdb/influxdb/influxql"
	"github.com/influxdb/influxdb/meta"
	"github.com/influxdb/influxdb/tsdb"
	"github.com/influxdb/influxdb/uuid"
)

const (
	// With raw data queries, mappers will read up to this amount before sending results back to the engine.
	// This is the default size in the number of values returned in a raw query. Could be many more bytes depending on fields returned.
	DefaultChunkSize = 10000
)

// TODO: Standard response headers (see: HeaderHandler)
// TODO: Compression (see: CompressionHeaderHandler)

// TODO: Check HTTP response codes: 400, 401, 403, 409.

type route struct {
	name        string
	method      string
	pattern     string
	gzipped     bool
	log         bool
	handlerFunc interface{}
}

// Handler represents an HTTP handler for the InfluxDB server.
type Handler struct {
	mux                   *pat.PatternServeMux
	requireAuthentication bool
	version               string

	MetaStore interface {
		Database(name string) (*meta.DatabaseInfo, error)
		Authenticate(username, password string) (ui *meta.UserInfo, err error)
		Users() ([]meta.UserInfo, error)
	}

	QueryExecutor interface {
		ExecuteQuery(q *influxql.Query, db string, user *meta.UserInfo, chunkSize int) (<-chan *influxql.Result, error)
	}

	SeriesWriter interface {
		WriteSeries(database, retentionPolicy string, points []tsdb.Point) error
	}

	Logger         *log.Logger
	loggingEnabled bool // Log every HTTP access.
	WriteTrace     bool // Detailed logging of write path
}

// NewHandler returns a new instance of handler with routes.
func NewHandler(requireAuthentication, loggingEnabled bool, version string) *Handler {
	h := &Handler{
		mux: pat.New(),
		requireAuthentication: requireAuthentication,
		Logger:                log.New(os.Stderr, "[http] ", log.LstdFlags),
		loggingEnabled:        loggingEnabled,
		version:               version,
	}

	h.SetRoutes([]route{
		route{
			"query", // Query serving route.
			"GET", "/query", true, true, h.serveQuery,
		},
		route{
			"write", // Data-ingest route.
			"OPTIONS", "/write", true, true, h.serveOptions,
		},
		route{
			"write", // Data-ingest route.
			"POST", "/write", true, true, h.serveWrite,
		},
		route{ // Ping
			"ping",
			"GET", "/ping", true, true, h.servePing,
		},
		route{ // Ping
			"ping-head",
			"HEAD", "/ping", true, true, h.servePing,
		},
		// route{
		// 	"dump", // export all points in the given db.
		// 	"GET", "/dump", true, true, h.serveDump,
		// },
	})

	return h
}

func (h *Handler) SetRoutes(routes []route) {
	for _, r := range routes {
		var handler http.Handler

		// If it's a handler func that requires authorization, wrap it in authorization
		if hf, ok := r.handlerFunc.(func(http.ResponseWriter, *http.Request, *meta.UserInfo)); ok {
			handler = authenticate(hf, h, h.requireAuthentication)
		}
		// This is a normal handler signature and does not require authorization
		if hf, ok := r.handlerFunc.(func(http.ResponseWriter, *http.Request)); ok {
			handler = http.HandlerFunc(hf)
		}

		if r.gzipped {
			handler = gzipFilter(handler)
		}
		handler = versionHeader(handler, h.version)
		handler = cors(handler)
		handler = requestID(handler)
		if h.loggingEnabled && r.log {
			handler = logging(handler, r.name, h.Logger)
		}
		handler = recovery(handler, r.name, h.Logger) // make sure recovery is always last

		h.mux.Add(r.method, r.pattern, handler)
	}
}

// ServeHTTP responds to HTTP request to the handler.
func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	// FIXME(benbjohnson): Add pprof enabled flag.
	if strings.HasPrefix(r.URL.Path, "/debug/pprof") {
		switch r.URL.Path {
		case "/debug/pprof/cmdline":
			pprof.Cmdline(w, r)
		case "/debug/pprof/profile":
			pprof.Profile(w, r)
		case "/debug/pprof/symbol":
			pprof.Symbol(w, r)
		default:
			pprof.Index(w, r)
		}
		return
	}

	h.mux.ServeHTTP(w, r)
}

// serveQuery parses an incoming query and, if valid, executes the query.
func (h *Handler) serveQuery(w http.ResponseWriter, r *http.Request, user *meta.UserInfo) {
	q := r.URL.Query()
	pretty := q.Get("pretty") == "true"

	qp := strings.TrimSpace(q.Get("q"))
	if qp == "" {
		httpError(w, `missing required parameter "q"`, pretty, http.StatusBadRequest)
		return
	}

	p := influxql.NewParser(strings.NewReader(qp))
	db := q.Get("db")

	// Parse query from query string.
	query, err := p.ParseQuery()
	if err != nil {
		httpError(w, "error parsing query: "+err.Error(), pretty, http.StatusBadRequest)
		return
	}

	// Parse chunk size. Use default if not provided or unparsable.
	chunked := (q.Get("chunked") == "true")
	chunkSize := DefaultChunkSize
	if chunked {
		if n, err := strconv.ParseInt(q.Get("chunk_size"), 10, 64); err == nil {
			chunkSize = int(n)
		}
	}

	// Execute query.
	w.Header().Add("content-type", "application/json")
	results, err := h.QueryExecutor.ExecuteQuery(query, db, user, chunkSize)

	if _, ok := err.(meta.AuthError); ok {
		w.WriteHeader(http.StatusUnauthorized)
		return
	} else if err != nil {
		w.WriteHeader(http.StatusInternalServerError)
		return
	}

	// if we're not chunking, this will be the in memory buffer for all results before sending to client
	resp := influxdb.Response{Results: make([]*influxql.Result, 0)}
	statusWritten := false

	// pull all results from the channel
	for r := range results {
		// write the status header based on the first result returned in the channel
		if !statusWritten {
			status := http.StatusOK

			if r != nil && r.Err != nil {
				if _, ok := r.Err.(meta.AuthError); ok {
					status = http.StatusUnauthorized
				}
			}

			w.WriteHeader(status)
			statusWritten = true
		}

		// Ignore nil results.
		if r == nil {
			continue
		}

		// Write out result immediately if chunked.
		if chunked {
			w.Write(MarshalJSON(influxdb.Response{
				Results: []*influxql.Result{r},
			}, pretty))
			w.(http.Flusher).Flush()
			continue
		}

		// It's not chunked so buffer results in memory.
		// Results for statements need to be combined together.
		// We need to check if this new result is for the same statement as
		// the last result, or for the next statement
		l := len(resp.Results)
		if l == 0 {
			resp.Results = append(resp.Results, r)
		} else if resp.Results[l-1].StatementID == r.StatementID {
			cr := resp.Results[l-1]
			cr.Series = append(cr.Series, r.Series...)
		} else {
			resp.Results = append(resp.Results, r)
		}
	}

	// If it's not chunked we buffered everything in memory, so write it out
	if !chunked {
		w.Write(MarshalJSON(resp, pretty))
	}
}

// serveWrite receives incoming series data and writes it to the database.
func (h *Handler) serveWrite(w http.ResponseWriter, r *http.Request, user *meta.UserInfo) {
	// Check to see if we have a gzip'd post
	var body io.ReadCloser
	if r.Header.Get("Content-encoding") == "gzip" {
		b, err := gzip.NewReader(r.Body)
		if err != nil {
			resultError(w, influxql.Result{Err: err}, http.StatusBadRequest)
			return
		}
		body = b
		defer r.Body.Close()
	} else {
		body = r.Body
	}

	var bp client.BatchPoints
	var dec *json.Decoder

	if h.WriteTrace {
		b, err := ioutil.ReadAll(body)
		if err != nil {
			h.Logger.Print("write handler failed to read bytes from request body")
		} else {
			h.Logger.Printf("write body received by handler: %s", string(b))
		}
		dec = json.NewDecoder(strings.NewReader(string(b)))
	} else {
		dec = json.NewDecoder(body)
		defer body.Close()
	}

	if err := dec.Decode(&bp); err != nil {
		if err.Error() == "EOF" {
			w.WriteHeader(http.StatusOK)
			return
		}
		resultError(w, influxql.Result{Err: err}, http.StatusBadRequest)
		return
	}

	if bp.Database == "" {
		resultError(w, influxql.Result{Err: fmt.Errorf("database is required")}, http.StatusBadRequest)
		return
	}

	if di, err := h.MetaStore.Database(bp.Database); err != nil {
		resultError(w, influxql.Result{Err: fmt.Errorf("metastore database error: %s", err)}, http.StatusInternalServerError)
		return
	} else if di == nil {
		resultError(w, influxql.Result{Err: fmt.Errorf("database not found: %q", bp.Database)}, http.StatusNotFound)
		return
	}

	if h.requireAuthentication && user == nil {
		resultError(w, influxql.Result{Err: fmt.Errorf("user is required to write to database %q", bp.Database)}, http.StatusUnauthorized)
		return
	}

	if h.requireAuthentication && !user.Authorize(influxql.WritePrivilege, bp.Database) {
		resultError(w, influxql.Result{Err: fmt.Errorf("%q user is not authorized to write to database %q", user.Name, bp.Database)}, http.StatusUnauthorized)
		return
	}

	points, err := influxdb.NormalizeBatchPoints(bp)
	if err != nil {
		resultError(w, influxql.Result{Err: err}, http.StatusInternalServerError)
		return
	}

	if err := h.SeriesWriter.WriteSeries(bp.Database, bp.RetentionPolicy, points); err != nil {
		if influxdb.IsClientError(err) {
			resultError(w, influxql.Result{Err: err}, http.StatusBadRequest)
		} else {
			resultError(w, influxql.Result{Err: err}, http.StatusInternalServerError)
		}
		return
	}

	w.WriteHeader(http.StatusNoContent)
}

// serveOptions returns an empty response to comply with OPTIONS pre-flight requests
func (h *Handler) serveOptions(w http.ResponseWriter, r *http.Request) {
	w.WriteHeader(http.StatusNoContent)
}

// servePing returns a simple response to let the client know the server is running.
func (h *Handler) servePing(w http.ResponseWriter, r *http.Request) {
	w.WriteHeader(http.StatusNoContent)
}

// MarshalJSON will marshal v to JSON. Pretty prints if pretty is true.
func MarshalJSON(v interface{}, pretty bool) []byte {
	var b []byte
	var err error
	if pretty {
		b, err = json.MarshalIndent(v, "", "    ")
	} else {
		b, err = json.Marshal(v)
	}

	if err != nil {
		return []byte(err.Error())
	}
	return b
}

type Point struct {
	Name   string                 `json:"name"`
	Time   time.Time              `json:"time"`
	Tags   map[string]string      `json:"tags"`
	Fields map[string]interface{} `json:"fields"`
}

type Batch struct {
	Database        string  `json:"database"`
	RetentionPolicy string  `json:"retentionPolicy"`
	Points          []Point `json:"points"`
}

// httpError writes an error to the client in a standard format.
func httpError(w http.ResponseWriter, error string, pretty bool, code int) {
	w.Header().Add("content-type", "application/json")
	w.WriteHeader(code)

	response := influxdb.Response{Err: errors.New(error)}
	var b []byte
	if pretty {
		b, _ = json.MarshalIndent(response, "", "    ")
	} else {
		b, _ = json.Marshal(response)
	}
	w.Write(b)
}

func resultError(w http.ResponseWriter, result influxql.Result, code int) {
	w.Header().Add("content-type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(&result)
}

// Filters and filter helpers

// parseCredentials returns the username and password encoded in
// a request. The credentials may be present as URL query params, or as
// a Basic Authentication header.
// as params: http://127.0.0.1/query?u=username&p=password
// as basic auth: http://username:password@127.0.0.1
func parseCredentials(r *http.Request) (string, string, error) {
	q := r.URL.Query()

	if u, p := q.Get("u"), q.Get("p"); u != "" && p != "" {
		return u, p, nil
	}
	if u, p, ok := r.BasicAuth(); ok {
		return u, p, nil
	} else {
		return "", "", fmt.Errorf("unable to parse Basic Auth credentials")
	}
}

// authenticate wraps a handler and ensures that if user credentials are passed in
// an attempt is made to authenticate that user. If authentication fails, an error is returned.
//
// There is one exception: if there are no users in the system, authentication is not required. This
// is to facilitate bootstrapping of a system with authentication enabled.
func authenticate(inner func(http.ResponseWriter, *http.Request, *meta.UserInfo), h *Handler, requireAuthentication bool) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Return early if we are not authenticating
		if !requireAuthentication {
			inner(w, r, nil)
			return
		}
		var user *meta.UserInfo

		// Retrieve user list.
		uis, err := h.MetaStore.Users()
		if err != nil {
			httpError(w, err.Error(), false, http.StatusInternalServerError)
			return
		}

		// TODO corylanou: never allow this in the future without users
		if requireAuthentication && len(uis) > 0 {
			username, password, err := parseCredentials(r)
			if err != nil {
				httpError(w, err.Error(), false, http.StatusUnauthorized)
				return
			}
			if username == "" {
				httpError(w, "username required", false, http.StatusUnauthorized)
				return
			}

			user, err = h.MetaStore.Authenticate(username, password)
			if err != nil {
				httpError(w, err.Error(), false, http.StatusUnauthorized)
				return
			}
		}
		inner(w, r, user)
	})
}

type gzipResponseWriter struct {
	io.Writer
	http.ResponseWriter
}

func (w gzipResponseWriter) Write(b []byte) (int, error) {
	return w.Writer.Write(b)
}

func (w gzipResponseWriter) Flush() {
	w.Writer.(*gzip.Writer).Flush()
}

// determines if the client can accept compressed responses, and encodes accordingly
func gzipFilter(inner http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.Contains(r.Header.Get("Accept-Encoding"), "gzip") {
			inner.ServeHTTP(w, r)
			return
		}
		w.Header().Set("Content-Encoding", "gzip")
		gz := gzip.NewWriter(w)
		defer gz.Close()
		gzw := gzipResponseWriter{Writer: gz, ResponseWriter: w}
		inner.ServeHTTP(gzw, r)
	})
}

// versionHeader taks a HTTP handler and returns a HTTP handler
// and adds the X-INFLUXBD-VERSION header to outgoing responses.
func versionHeader(inner http.Handler, version string) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Add("X-InfluxDB-Version", version)
		inner.ServeHTTP(w, r)
	})
}

// cors responds to incoming requests and adds the appropriate cors headers
// TODO: corylanou: add the ability to configure this in our config
func cors(inner http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if origin := r.Header.Get("Origin"); origin != "" {
			w.Header().Set(`Access-Control-Allow-Origin`, origin)
			w.Header().Set(`Access-Control-Allow-Methods`, strings.Join([]string{
				`DELETE`,
				`GET`,
				`OPTIONS`,
				`POST`,
				`PUT`,
			}, ", "))

			w.Header().Set(`Access-Control-Allow-Headers`, strings.Join([]string{
				`Accept`,
				`Accept-Encoding`,
				`Authorization`,
				`Content-Length`,
				`Content-Type`,
				`X-CSRF-Token`,
				`X-HTTP-Method-Override`,
			}, ", "))
		}

		if r.Method == "OPTIONS" {
			return
		}

		inner.ServeHTTP(w, r)
	})
}

func requestID(inner http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		uid := uuid.TimeUUID()
		r.Header.Set("Request-Id", uid.String())
		w.Header().Set("Request-Id", r.Header.Get("Request-Id"))

		inner.ServeHTTP(w, r)
	})
}

func logging(inner http.Handler, name string, weblog *log.Logger) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		l := &responseLogger{w: w}
		inner.ServeHTTP(l, r)
		logLine := buildLogLine(l, r, start)
		weblog.Println(logLine)
	})
}

func recovery(inner http.Handler, name string, weblog *log.Logger) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		l := &responseLogger{w: w}
		inner.ServeHTTP(l, r)
		if err := recover(); err != nil {
			logLine := buildLogLine(l, r, start)
			logLine = fmt.Sprintf(`%s [err:%s]`, logLine, err)
			weblog.Println(logLine)
		}
	})
}

/*
FIXME: Convert to line protocol format.

// serveDump returns all points in the given database as a plaintext list of JSON structs.
// To get all points:
// Find all measurements (show measurements).
// For each measurement do select * from <measurement> group by *
func (h *Handler) serveDump(w http.ResponseWriter, r *http.Request, user *meta.UserInfo) {
	q := r.URL.Query()
	db := q.Get("db")
	pretty := q.Get("pretty") == "true"
	delim := []byte("\n")
	measurements, err := h.showMeasurements(db, user)
	if err != nil {
		httpError(w, "error with dump: "+err.Error(), pretty, http.StatusInternalServerError)
		return
	}

	// Fetch all the points for each measurement.
	// From the 'select' query below, we get:
	//
	// columns:[col1, col2, col3, ...]
	// - and -
	// values:[[val1, val2, val3, ...], [val1, val2, val3, ...], [val1, val2, val3, ...]...]
	//
	// We need to turn that into multiple rows like so...
	// fields:{col1 : values[0][0], col2 : values[0][1], col3 : values[0][2]}
	// fields:{col1 : values[1][0], col2 : values[1][1], col3 : values[1][2]}
	// fields:{col1 : values[2][0], col2 : values[2][1], col3 : values[2][2]}
	//
	for _, measurement := range measurements {
		queryString := fmt.Sprintf("select * from %s group by *", measurement)
		p := influxql.NewParser(strings.NewReader(queryString))
		query, err := p.ParseQuery()
		if err != nil {
			httpError(w, "error with dump: "+err.Error(), pretty, http.StatusInternalServerError)
			return
		}

		res, err := h.QueryExecutor.ExecuteQuery(query, db, user, DefaultChunkSize)
		if err != nil {
			w.Write([]byte("*** SERVER-SIDE ERROR. MISSING DATA ***"))
			w.Write(delim)
			return
		}
		for result := range res {
			for _, row := range result.Series {
				points := make([]Point, 1)
				var point Point
				point.Name = row.Name
				point.Tags = row.Tags
				point.Fields = make(map[string]interface{})
				for _, tuple := range row.Values {
					for subscript, cell := range tuple {
						if row.Columns[subscript] == "time" {
							point.Time, _ = cell.(time.Time)
							continue
						}
						point.Fields[row.Columns[subscript]] = cell
					}
					points[0] = point
					batch := &Batch{
						Points:          points,
						Database:        db,
						RetentionPolicy: "default",
					}
					buf, err := json.Marshal(&batch)

					// TODO: Make this more legit in the future
					// Since we're streaming data as chunked responses, this error could
					// be in the middle of an already-started data stream. Until Go 1.5,
					// we can't really support proper trailer headers, so we'll just
					// wait until then: https://code.google.com/p/go/issues/detail?id=7759
					if err != nil {
						w.Write([]byte("*** SERVER-SIDE ERROR. MISSING DATA ***"))
						w.Write(delim)
						return
					}
					w.Write(buf)
					w.Write(delim)
				}
			}
		}
	}
}

// Return all the measurements from the given DB
func (h *Handler) showMeasurements(db string, user *meta.UserInfo) ([]string, error) {
	var measurements []string
	c, err := h.QueryExecutor.ExecuteQuery(&influxql.Query{Statements: []influxql.Statement{&influxql.ShowMeasurementsStatement{}}}, db, user, 0)
	if err != nil {
		return measurements, err
	}
	results := influxdb.Response{}

	for r := range c {
		results.Results = append(results.Results, r)
	}

	for _, result := range results.Results {
		for _, row := range result.Series {
			for _, tuple := range (*row).Values {
				for _, cell := range tuple {
					measurements = append(measurements, interfaceToString(cell))
				}
			}
		}
	}
	return measurements, nil
}

func interfaceToString(v interface{}) string {
	switch t := v.(type) {
	case nil:
		return ""
	case int, int8, int16, int32, int64, uint, uint8, uint16, uint32, uint64, uintptr:
		return fmt.Sprintf("%d", t)
	default:
		return fmt.Sprintf("%v", t)
	}
}
*/
