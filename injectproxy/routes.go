// Copyright 2020 The Prometheus Authors
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
// http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package injectproxy

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"log/slog"
	"net/http"
	"net/http/httputil"
	"net/url"
	"regexp"
	"sort"
	"strings"

	"crypto/sha256"
	"encoding/hex"

	"github.com/efficientgo/core/merrors"
	"github.com/metalmatze/signal/server/signalhttp"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/prometheus/model/labels"
	"github.com/prometheus/prometheus/promql/parser"
)

const (
	queryParam    = "query"
	matchersParam = "match[]"
)

type routes struct {
	upstream *url.URL
	handler  http.Handler
	label    string
	el       ExtractLabeler

	mux                   http.Handler
	modifiers             map[string]func(*http.Response) error
	errorOnReplace        bool
	regexMatch            bool
	rulesWithActiveAlerts bool

	logger *log.Logger
}

type options struct {
	enableLabelAPIs       bool
	passthroughPaths      []string
	errorOnReplace        bool
	registerer            prometheus.Registerer
	regexMatch            bool
	rulesWithActiveAlerts bool
	onlyMetrics           bool
	rewriteMetricsPath    string
}

type Option interface {
	apply(*options)
}

type optionFunc func(*options)

func (f optionFunc) apply(o *options) {
	f(o)
}

// WithPrometheusRegistry configures the proxy to use the given registerer.
func WithPrometheusRegistry(reg prometheus.Registerer) Option {
	return optionFunc(func(o *options) {
		o.registerer = reg
	})
}

// WithEnabledLabelsAPI enables proxying to labels API. If false, "501 Not implemented" will be return for those.
func WithEnabledLabelsAPI() Option {
	return optionFunc(func(o *options) {
		o.enableLabelAPIs = true
	})
}

func WithRewriteMetricsPath(path string) Option {
	return optionFunc(func(o *options) {
		o.rewriteMetricsPath = path
	})
}

// WithPassthroughPaths configures routes to register given paths as passthrough handlers for all HTTP methods.
// that, if requested, will be forwarded without enforcing label. Use with care.
// NOTE: Passthrough "all" paths like "/" or "" and regex are not allowed.
func WithPassthroughPaths(paths []string) Option {
	return optionFunc(func(o *options) {
		o.passthroughPaths = paths
	})
}

// WithErrorOnReplace causes the proxy to return 400 if a label matcher we want to
// inject is present in the query already and matches something different
func WithErrorOnReplace() Option {
	return optionFunc(func(o *options) {
		o.errorOnReplace = true
	})
}

// WithActiveAlerts causes the proxy to return rules with active alerts.
func WithActiveAlerts() Option {
	return optionFunc(func(o *options) {
		o.rulesWithActiveAlerts = true
	})
}

// WithRegexMatch causes the proxy to handle tenant name as regexp
func WithRegexMatch() Option {
	return optionFunc(func(o *options) {
		o.regexMatch = true
	})
}

// onlyMetrics allow only /metrics api
func WithOnlyMetrics() Option {
	return optionFunc(func(o *options) {
		o.onlyMetrics = true
	})
}

// mux abstracts away the behavior we expect from the http.ServeMux type in this package.
type mux interface {
	http.Handler
	Handle(string, http.Handler)
}

// strictMux is a mux that wraps standard HTTP handler with safer handler that allows safe user provided handler registrations.
type strictMux struct {
	mux
	seen map[string]struct{}
}

func newStrictMux(m mux) *strictMux {
	return &strictMux{
		m,
		map[string]struct{}{},
	}

}

// Handle is like HTTP mux handle but it does not allow to register paths that are shared with previously registered paths.
// It also makes sure the trailing / is registered too.
// For example if /api/v1/federate was registered consequent registrations like /api/v1/federate/ or /api/v1/federate/some will
// return error. In the mean time request with both /api/v1/federate and /api/v1/federate/ will point to the handled passed by /api/v1/federate
// registration.
// This allows to de-risk ability for user to mis-configure and leak inject isolation.
func (s *strictMux) Handle(pattern string, handler http.Handler) error {
	sanitized := pattern
	for next := strings.TrimSuffix(sanitized, "/"); next != sanitized; sanitized = next {
	}

	if _, ok := s.seen[sanitized]; ok {
		return fmt.Errorf("pattern %q was already registered", sanitized)
	}

	for p := range s.seen {
		if strings.HasPrefix(sanitized+"/", p+"/") {
			return fmt.Errorf("pattern %q is registered, cannot register path %q that shares it", p, sanitized)
		}
	}

	s.mux.Handle(sanitized, handler)
	s.mux.Handle(sanitized+"/", handler)
	s.seen[sanitized] = struct{}{}

	return nil
}

// instrumentedMux wraps a mux and instruments it.
type instrumentedMux struct {
	mux
	i signalhttp.HandlerInstrumenter
}

func newInstrumentedMux(m mux, r prometheus.Registerer) *instrumentedMux {
	return &instrumentedMux{
		m,
		signalhttp.NewHandlerInstrumenter(r, []string{"handler"}),
	}
}

// Handle implements the mux interface.
func (i *instrumentedMux) Handle(pattern string, handler http.Handler) {
	i.mux.Handle(pattern, i.i.NewHandler(prometheus.Labels{"handler": pattern}, handler))
}

// ExtractLabeler is an HTTP handler that extract the label value to be
// enforced from the HTTP request.  If a valid label value is found, it should
// store it in the request's context.  Otherwise it should return an error in
// the HTTP response (usually 400 or 500).
type ExtractLabeler interface {
	ExtractLabel(next http.HandlerFunc) http.Handler
}

// HTTPFormEnforcer enforces a label value extracted from the HTTP form and query parameters.
type HTTPFormEnforcer struct {
	ParameterName string
	ValueRegexp   string
	ResultFString string
}

// ExtractLabel implements the ExtractLabeler interface.
func (hff HTTPFormEnforcer) ExtractLabel(next http.HandlerFunc) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		labelValues, err := hff.getLabelValues(r)
		if err != nil {
			prometheusAPIError(w, humanFriendlyErrorMessage(err), http.StatusBadRequest)
			return
		}

		// Remove the proxy label from the query parameters.
		q := r.URL.Query()
		q.Del(hff.ParameterName)
		r.URL.RawQuery = q.Encode()

		// Remove the param from the PostForm.
		if r.Method == http.MethodPost {
			if err := r.ParseForm(); err != nil {
				prometheusAPIError(w, fmt.Sprintf("Failed to parse the PostForm: %v", err), http.StatusInternalServerError)
				return
			}
			if r.PostForm.Get(hff.ParameterName) != "" {
				r.PostForm.Del(hff.ParameterName)
				newBody := r.PostForm.Encode()
				// We are replacing request body, close previous one (r.FormValue ensures it is read fully and not nil).
				_ = r.Body.Close()
				r.Body = io.NopCloser(strings.NewReader(newBody))
				r.ContentLength = int64(len(newBody))
			}
		}

		next.ServeHTTP(w, r.WithContext(WithLabelValues(r.Context(), labelValues)))
	})
}

func (hff HTTPFormEnforcer) getLabelValues(r *http.Request) ([]string, error) {
	err := r.ParseForm()
	if err != nil {
		return nil, fmt.Errorf("the form data can not be parsed: %w", err)
	}

	formValues := removeEmptyValues(r.Form[hff.ParameterName])

	if hff.ValueRegexp != "" {
		formValues = parseValues(formValues, hff.ValueRegexp, hff.ResultFString)
	}

	if len(formValues) == 0 {
		return nil, fmt.Errorf("the %q query parameter must be provided", hff.ParameterName)
	}

	return formValues, nil
}

// HTTPHeaderEnforcer enforces a label value extracted from the HTTP headers.
type HTTPHeaderEnforcer struct {
	Name            string
	ParseListSyntax bool
	ValueRegexp     string
	ResultFString   string
	Cache           *Cache
	MetricsNames    []string
}

type responseWriter struct {
	http.ResponseWriter
	statusCode int
	body       *bytes.Buffer
}

func newResponseWriter(w http.ResponseWriter) *responseWriter {
	return &responseWriter{
		ResponseWriter: w,
		statusCode:     http.StatusOK, // Default status code
		body:           &bytes.Buffer{},
	}
}

// Override the WriteHeader method to capture the status code
func (rw *responseWriter) WriteHeader(code int) {
	rw.statusCode = code
	rw.ResponseWriter.WriteHeader(code)
}

// Override the Write method to capture the response body
func (rw *responseWriter) Write(b []byte) (int, error) {

	return rw.body.Write(b)
	//return // rw.ResponseWriter.Write(b)

}

func generateCacheKey(labelValues []string) string {
	// Join the label values into a single string
	joinedValues := strings.Join(labelValues, ",")

	// Hash the joined string to create a unique key
	hash := sha256.New()
	hash.Write([]byte(joinedValues))
	return hex.EncodeToString(hash.Sum(nil))
}

func trimMetricsByNames(data []byte, metricNames []string) ([]byte, error) {

	lines := strings.Split(string(data), "\n")

	var filteredMetrics bytes.Buffer

	// Create a map for quick lookup of metric names
	metricMap := make(map[string]struct{})
	for _, name := range metricNames {
		metricMap[name] = struct{}{}
	}

	isNext := false
	for _, line := range lines {
		if len(line) == 0 {
			continue // Skip empty lines
		}

		// Check if the line starts with a metric type or metric name
		if strings.HasPrefix(line, "# TYPE") {

			parts := strings.Fields(line)

			if len(parts) > 1 {
				metricName := parts[2]
				if _, ok := metricMap[metricName]; ok {
					filteredMetrics.WriteString(line + "\n")
					isNext = true
				} else {
					isNext = false
				}
			} else {

				isNext = false
			}
		} else {
			if isNext {
				filteredMetrics.WriteString(line + "\n")
			}
		}

	}

	return filteredMetrics.Bytes(), nil
}

// ExtractLabel implements the ExtractLabeler interface.
func (hhe HTTPHeaderEnforcer) ExtractLabel(next http.HandlerFunc) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		labelValues, err := hhe.getLabelValues(r)
		if err != nil {
			prometheusAPIError(w, humanFriendlyErrorMessage(err), http.StatusBadRequest)
			return
		}

		cacheKey := generateCacheKey(labelValues)

		// Check cache and return or redirect to prometheus
		if prometheusCacheAPIResponse(w, cacheKey, hhe.Cache) == nil {
			return
		}

		rw := newResponseWriter(w)
		next.ServeHTTP(rw, r.WithContext(WithLabelValues(r.Context(), labelValues)))

		// trim reponse by labels list
		// Write the response to the cache
		if rw.statusCode == http.StatusOK {

			body := rw.body.Bytes()

			// If MetricsNames provided then trim it
			if len(hhe.MetricsNames) > 0 && r.URL.Path == "/federate" {

				body, err = trimMetricsByNames(rw.body.Bytes(), hhe.MetricsNames)
				if err != nil {
					prometheusAPIError(w, humanFriendlyErrorMessage(err), http.StatusBadRequest)
					return
				}
			}

			if _, err := rw.ResponseWriter.Write(body); err == nil {
				hhe.Cache.Set(cacheKey, body)
			}
		}

	})
}

func (hhe HTTPHeaderEnforcer) getLabelValues(r *http.Request) ([]string, error) {
	headerValues := r.Header[hhe.Name]

	slog.Debug("getLabelValues", "headerValues", headerValues)

	if hhe.ParseListSyntax {
		headerValues = trimValues(splitValues(headerValues, ","))
	}

	headerValues = removeEmptyValues(headerValues)

	slog.Debug("getLabelValues", "ValueRegexp", hhe.ValueRegexp)

	if hhe.ValueRegexp != "" {
		headerValues = parseValues(headerValues, hhe.ValueRegexp, hhe.ResultFString)
	}

	slog.Debug("getLabelValues", "headerValues", headerValues)

	if len(headerValues) == 0 {
		return nil, fmt.Errorf("missing HTTP header %q", hhe.Name)
	}

	return headerValues, nil
}

// StaticLabelEnforcer enforces a static label value., ResultFString
type StaticLabelEnforcer []string

// the ExtractLabeler interface.
func (sle StaticLabelEnforcer) ExtractLabel(next http.HandlerFunc) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		next(w, r.WithContext(WithLabelValues(r.Context(), sle)))
	})
}

func NewRoutes(upstream *url.URL, label string, extractLabeler ExtractLabeler, opts ...Option) (*routes, error) {
	opt := options{}
	for _, o := range opts {
		o.apply(&opt)
	}

	if opt.registerer == nil {
		opt.registerer = prometheus.NewRegistry()
	}

	proxy := httputil.NewSingleHostReverseProxy(upstream)

	// Add origin host to proxy request
	proxy.Director = func(req *http.Request) {
		// Set the Host header
		req.Host = upstream.Host
		req.URL.Scheme = upstream.Scheme
		req.URL.Host = upstream.Host

		// Set proxy path back to federate
		if opt.rewriteMetricsPath != "" && opt.rewriteMetricsPath == req.URL.Path {
			req.URL.Path = "/federate"
		}
	}

	r := &routes{
		upstream:              upstream,
		handler:               proxy,
		label:                 label,
		el:                    extractLabeler,
		errorOnReplace:        opt.errorOnReplace,
		regexMatch:            opt.regexMatch,
		rulesWithActiveAlerts: opt.rulesWithActiveAlerts,
		logger:                log.Default(),
	}
	mux := newStrictMux(newInstrumentedMux(http.NewServeMux(), opt.registerer))

	mpath := "/metrics"
	if opt.rewriteMetricsPath != "" {
		mpath = opt.rewriteMetricsPath
	}

	slog.Debug("Expose API", "path for metrics", mpath)

	errs := merrors.New(
		mux.Handle(mpath, r.el.ExtractLabel(enforceMethods(r.matcher, "GET"))),
	)

	slog.Info("Expose API", "onlyMetrics", opt.onlyMetrics)

	// if onlyMetrics is set to false then expose other APIs
	if !opt.onlyMetrics {
		errs.Add(
			mux.Handle("/federate", r.el.ExtractLabel(enforceMethods(r.matcher, "GET"))),
			mux.Handle("/api/v1/query", r.el.ExtractLabel(enforceMethods(r.query, "GET", "POST"))),
			mux.Handle("/api/v1/query_range", r.el.ExtractLabel(enforceMethods(r.query, "GET", "POST"))),
			mux.Handle("/api/v1/alerts", r.el.ExtractLabel(enforceMethods(r.passthrough, "GET"))),
			mux.Handle("/api/v1/rules", r.el.ExtractLabel(enforceMethods(r.passthrough, "GET"))),
			mux.Handle("/api/v1/series", r.el.ExtractLabel(enforceMethods(r.matcher, "GET", "POST"))),
			mux.Handle("/api/v1/query_exemplars", r.el.ExtractLabel(enforceMethods(r.query, "GET", "POST"))),
		)

		if opt.enableLabelAPIs {
			errs.Add(
				mux.Handle("/api/v1/labels", r.el.ExtractLabel(enforceMethods(r.matcher, "GET", "POST"))),
				// Full path is /api/v1/label/<label_name>/values but http mux does not support patterns.
				// This is fine though as we don't care about name for matcher injector.
				mux.Handle("/api/v1/label/", r.el.ExtractLabel(enforceMethods(r.matcher, "GET"))),
			)
		}

		errs.Add(
			// Reject multi label values with assertSingleLabelValue() because the
			// semantics of the Silences API don't support multi-label matchers.
			mux.Handle("/api/v2/silences", r.el.ExtractLabel(
				r.errorIfRegexpMatch(
					enforceMethods(
						assertSingleLabelValue(r.silences),
						"GET", "POST",
					),
				),
			)),
			mux.Handle("/api/v2/silence/", r.el.ExtractLabel(
				r.errorIfRegexpMatch(
					enforceMethods(
						assertSingleLabelValue(r.deleteSilence),
						"DELETE",
					),
				),
			)),
			mux.Handle("/api/v2/alerts/groups", r.el.ExtractLabel(enforceMethods(r.enforceFilterParameter, "GET"))),
			mux.Handle("/api/v2/alerts", r.el.ExtractLabel(enforceMethods(r.alerts, "GET"))),
		)
	}

	errs.Add(
		mux.Handle("/healthz", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			_ = json.NewEncoder(w).Encode(map[string]bool{"ok": true})
		})),
	)

	if err := errs.Err(); err != nil {
		return nil, err
	}

	// Validate paths.
	for _, path := range opt.passthroughPaths {
		u, err := url.Parse(fmt.Sprintf("http://example.com%v", path))
		if err != nil {
			return nil, fmt.Errorf("path %q is not a valid URI path, got %v", path, opt.passthroughPaths)
		}
		if u.Path != path {
			return nil, fmt.Errorf("path %q is not a valid URI path, got %v", path, opt.passthroughPaths)
		}
		if u.Path == "" || u.Path == "/" {
			return nil, fmt.Errorf("path %q is not allowed, got %v", u.Path, opt.passthroughPaths)
		}
	}

	// Register optional passthrough paths.
	for _, path := range opt.passthroughPaths {
		if err := mux.Handle(path, http.HandlerFunc(r.passthrough)); err != nil {
			return nil, err
		}
	}

	r.mux = mux
	r.modifiers = map[string]func(*http.Response) error{
		"/api/v1/rules":  modifyAPIResponse(r.filterRules),
		"/api/v1/alerts": modifyAPIResponse(r.filterAlerts),
	}
	proxy.ModifyResponse = r.ModifyResponse
	proxy.ErrorHandler = r.errorHandler
	proxy.ErrorLog = log.Default()

	return r, nil
}

func (r *routes) ServeHTTP(w http.ResponseWriter, req *http.Request) {
	r.mux.ServeHTTP(w, req)
}

func (r *routes) ModifyResponse(resp *http.Response) error {
	m, found := r.modifiers[resp.Request.URL.Path]
	if !found {
		// Return the server's response unmodified.
		return nil
	}

	return m(resp)
}

func (r *routes) errorHandler(rw http.ResponseWriter, _ *http.Request, err error) {
	r.logger.Printf("http: proxy error: %v", err)
	if errors.Is(err, errModifyResponseFailed) {
		rw.WriteHeader(http.StatusBadRequest)
	}

	rw.WriteHeader(http.StatusBadGateway)
}

func enforceMethods(h http.HandlerFunc, methods ...string) http.HandlerFunc {
	return func(w http.ResponseWriter, req *http.Request) {
		for _, m := range methods {
			if m == req.Method {
				h(w, req)
				return
			}
		}
		http.NotFound(w, req)
	}
}

func (r *routes) errorIfRegexpMatch(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, req *http.Request) {
		if r.regexMatch {
			prometheusAPIError(w, "support for regex match not implemented", http.StatusNotImplemented)
			return
		}

		next(w, req)
	}
}

type ctxKey int

const keyLabel ctxKey = iota

// MustLabelValues returns labels (previously stored using WithLabelValue())
// from the given context.
// It will panic if no label is found or the value is empty.
func MustLabelValues(ctx context.Context) []string {
	labels, ok := ctx.Value(keyLabel).([]string)
	if !ok {
		panic(fmt.Sprintf("can't find the %q value in the context", keyLabel))
	}
	if len(labels) == 0 {
		panic(fmt.Sprintf("empty %q value in the context", keyLabel))
	}

	sort.Strings(labels)

	return labels
}

// MustLabelValue returns the first (alphabetical order) label value previously
// stored using WithLabelValue() from the given context.
// Similar to MustLabelValues, it will panic if no label is found or the value
// is empty.
func MustLabelValue(ctx context.Context) string {
	v := MustLabelValues(ctx)
	return v[0]
}

func labelValuesToRegexpString(labelValues []string) string {
	lvs := make([]string, len(labelValues))
	for i := range labelValues {
		lvs[i] = regexp.QuoteMeta(labelValues[i])
	}

	return strings.Join(lvs, "|")
}

// WithLabelValues stores labels in the given context.
func WithLabelValues(ctx context.Context, labels []string) context.Context {
	return context.WithValue(ctx, keyLabel, labels)
}

func (r *routes) passthrough(w http.ResponseWriter, req *http.Request) {
	r.handler.ServeHTTP(w, req)
}

func (r *routes) query(w http.ResponseWriter, req *http.Request) {
	var matcher *labels.Matcher

	if len(MustLabelValues(req.Context())) > 1 {
		if r.regexMatch {
			prometheusAPIError(w, "Only one label value allowed with regex match", http.StatusBadRequest)
			return
		}

		matcher = &labels.Matcher{
			Name:  r.label,
			Type:  labels.MatchRegexp,
			Value: labelValuesToRegexpString(MustLabelValues(req.Context())),
		}
	} else {
		matcherType := labels.MatchEqual
		matcherValue := MustLabelValue(req.Context())
		if r.regexMatch {
			compiledRegex, err := regexp.Compile(matcherValue)
			if err != nil {
				prometheusAPIError(w, err.Error(), http.StatusBadRequest)
				return
			}
			if compiledRegex.MatchString("") {
				prometheusAPIError(w, "Regex should not match empty string", http.StatusBadRequest)
				return
			}
			matcherType = labels.MatchRegexp
		}

		matcher = &labels.Matcher{
			Name:  r.label,
			Type:  matcherType,
			Value: matcherValue,
		}
	}

	e := NewPromQLEnforcer(r.errorOnReplace, matcher)

	// The `query` can come in the URL query string and/or the POST body.
	// For this reason, we need to try to enforcing in both places.
	// Note: a POST request may include some values in the URL query string
	// and others in the body. If both locations include a `query`, then
	// enforce in both places.
	q, found1, err := enforceQueryValues(e, req.URL.Query())
	if err != nil {
		switch {
		case errors.Is(err, ErrIllegalLabelMatcher):
			prometheusAPIError(w, err.Error(), http.StatusBadRequest)
		case errors.Is(err, ErrQueryParse):
			prometheusAPIError(w, err.Error(), http.StatusBadRequest)
		case errors.Is(err, ErrEnforceLabel):
			prometheusAPIError(w, err.Error(), http.StatusInternalServerError)
		}

		return
	}
	req.URL.RawQuery = q

	var found2 bool
	// Enforce the query in the POST body if needed.
	if req.Method == http.MethodPost {
		if err := req.ParseForm(); err != nil {
			prometheusAPIError(w, err.Error(), http.StatusBadRequest)
		}
		q, found2, err = enforceQueryValues(e, req.PostForm)
		if err != nil {
			switch {
			case errors.Is(err, ErrIllegalLabelMatcher):
				prometheusAPIError(w, err.Error(), http.StatusBadRequest)
			case errors.Is(err, ErrQueryParse):
				prometheusAPIError(w, err.Error(), http.StatusBadRequest)
			case errors.Is(err, ErrEnforceLabel):
				prometheusAPIError(w, err.Error(), http.StatusInternalServerError)
			}

			return
		}

		// We are replacing request body, close previous one (ParseForm ensures it is read fully and not nil).
		_ = req.Body.Close()
		req.Body = io.NopCloser(strings.NewReader(q))
		req.ContentLength = int64(len(q))
	}

	// If no query was found, return early.
	if !found1 && !found2 {
		return
	}

	r.handler.ServeHTTP(w, req)
}

func enforceQueryValues(e *PromQLEnforcer, v url.Values) (values string, noQuery bool, err error) {
	// If no values were given or no query is present,
	// e.g. because the query came in the POST body
	// but the URL query string was passed, then finish early.
	if v.Get(queryParam) == "" {
		return v.Encode(), false, nil
	}

	q, err := e.Enforce(v.Get(queryParam))
	if err != nil {
		return "", true, err
	}

	v.Set(queryParam, q)

	return v.Encode(), true, nil
}

func (r *routes) newLabelMatcher(vals ...string) (*labels.Matcher, error) {
	if r.regexMatch {
		if len(vals) != 1 {
			return nil, errors.New("only one label value allowed with regex match")
		}

		re := vals[0]
		compiledRegex, err := regexp.Compile(re)
		if err != nil {
			return nil, fmt.Errorf("invalid regex: %w", err)
		}

		if compiledRegex.MatchString("") {
			return nil, errors.New("regex should not match empty string")
		}

		m, err := labels.NewMatcher(labels.MatchRegexp, r.label, re)
		if err != nil {
			return nil, err
		}

		return m, nil
	}

	if len(vals) == 1 {
		return &labels.Matcher{
			Name:  r.label,
			Type:  labels.MatchEqual,
			Value: vals[0],
		}, nil
	}

	m, err := labels.NewMatcher(labels.MatchRegexp, r.label, labelValuesToRegexpString(vals))
	if err != nil {
		return nil, err
	}

	return m, nil
}

// matcher modifies all the match[] HTTP parameters to match on the tenant label.
// If none was provided, a tenant label matcher matcher is injected.
// This works for non-query Prometheus API endpoints like /api/v1/series,
// /api/v1/label/<name>/values, /api/v1/labels and /federate which support
// multiple matchers.
// See e.g https://prometheus.io/docs/prometheus/latest/querying/api/#querying-metadata
func (r *routes) matcher(w http.ResponseWriter, req *http.Request) {
	matcher, err := r.newLabelMatcher(MustLabelValues(req.Context())...)
	if err != nil {
		prometheusAPIError(w, err.Error(), http.StatusBadRequest)
		return
	}

	q := req.URL.Query()

	slog.Debug("matcher request path", "path", req.URL.Path)
	slog.Debug("matcher request query", "q", q)

	if err := injectMatcher(q, matcher); err != nil {
		prometheusAPIError(w, err.Error(), http.StatusBadRequest)
		return
	}

	req.URL.RawQuery = q.Encode()
	slog.Debug("matcher request RawQuery", "q", req.URL.RawQuery)

	if req.Method == http.MethodPost {
		if err := req.ParseForm(); err != nil {
			return
		}

		q = req.PostForm
		if err := injectMatcher(q, matcher); err != nil {
			return
		}

		// We are replacing request body, close previous one (ParseForm ensures it is read fully and not nil).
		_ = req.Body.Close()
		newBody := q.Encode()
		req.Body = io.NopCloser(strings.NewReader(newBody))
		req.ContentLength = int64(len(newBody))
	}

	r.handler.ServeHTTP(w, req)
}

func injectMatcher(q url.Values, matcher *labels.Matcher) error {
	matchers := q[matchersParam]
	if len(matchers) == 0 {
		q.Set(matchersParam, matchersToString(matcher))
		return nil
	}

	// Inject label into existing matchers.
	for i, m := range matchers {
		ms, err := parser.ParseMetricSelector(m)
		if err != nil {
			return err
		}

		matchers[i] = matchersToString(append(ms, matcher)...)
	}
	q[matchersParam] = matchers

	return nil
}

func matchersToString(ms ...*labels.Matcher) string {
	var el []string
	for _, m := range ms {
		el = append(el, m.String())
	}
	return fmt.Sprintf("{%v}", strings.Join(el, ","))
}

// humanFriendlyErrorMessage returns an error message with a capitalized first letter
// and a punctuation at the end.
func humanFriendlyErrorMessage(err error) string {
	if err == nil {
		return ""
	}
	errMsg := err.Error()
	return fmt.Sprintf("%s%s.", strings.ToUpper(errMsg[:1]), errMsg[1:])
}

func splitValues(slice []string, sep string) []string {
	for i := 0; i < len(slice); {
		splitResult := strings.Split(slice[i], sep)

		slice = append(slice[:i], append(splitResult, slice[i+1:]...)...)

		i += len(splitResult)
	}

	return slice
}

func parseValues(slice []string, pattern string, fstring string) []string {

	slog.Debug("parseValues", "slice", slice, "pattern", pattern)

	result := []string{}

	re, err := regexp.Compile(pattern)
	if err != nil {
		return result
	}

	for i := 0; i < len(slice); i++ {

		matches := re.FindStringSubmatch(slice[i])

		slog.Debug("parseValues", "slice", slice[i], "matches", matches)

		word := matches[1]
		if fstring != "" {
			word = fmt.Sprintf(fstring, matches[1])
		}

		if len(matches) > 1 {
			result = append(result, word)

		}
	}

	return result
}

func removeEmptyValues(slice []string) []string {
	for i := 0; i < len(slice); i++ {
		if slice[i] == "" {
			slice = append(slice[:i], slice[i+1:]...)
			i--
		}
	}

	return slice
}

func trimValues(slice []string) []string {
	for i := 0; i < len(slice); i++ {
		slice[i] = strings.TrimSpace(slice[i])
	}

	return slice
}
