// SPDX-License-Identifier: AGPL-3.0-only
// Provenance-includes-location: https://github.com/cortexproject/cortex/blob/master/pkg/querier/queryrange/query_range.go
// Provenance-includes-license: Apache-2.0
// Provenance-includes-copyright: The Cortex Authors.

package queryrange

import (
	"bytes"
	"context"
	stdjson "encoding/json"
	"fmt"
	"io/ioutil"
	"math"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"time"
	"unsafe"

	"github.com/go-kit/log"
	"github.com/gogo/protobuf/proto"
	"github.com/gogo/status"
	jsoniter "github.com/json-iterator/go"
	"github.com/opentracing/opentracing-go"
	otlog "github.com/opentracing/opentracing-go/log"
	"github.com/prometheus/common/model"
	"github.com/prometheus/prometheus/model/timestamp"
	"github.com/weaveworks/common/httpgrpc"

	apierror "github.com/grafana/mimir/pkg/api/error"
	"github.com/grafana/mimir/pkg/mimirpb"
	"github.com/grafana/mimir/pkg/util"
	"github.com/grafana/mimir/pkg/util/spanlogger"
)

// StatusSuccess Prometheus success result.
const StatusSuccess = "success"

var (
	matrix = model.ValMatrix.String()
	json   = jsoniter.Config{
		EscapeHTML:             false, // No HTML in our responses.
		SortMapKeys:            true,
		ValidateJsonRawMessage: true,
	}.Froze()
	errEndBeforeStart = apierror.New(apierror.TypeBadData, `invalid parameter "end": end timestamp must not be before start time`)
	errNegativeStep   = apierror.New(apierror.TypeBadData, `invalid parameter "step": zero or negative query resolution step widths are not accepted. Try a positive integer`)
	errStepTooSmall   = apierror.New(apierror.TypeBadData, "exceeded maximum resolution of 11,000 points per timeseries. Try decreasing the query resolution (?step=XX)")

	// PrometheusCodec is a codec to encode and decode Prometheus query range requests and responses.
	PrometheusCodec Codec = &prometheusCodec{}

	// Name of the cache control header.
	cacheControlHeader = "Cache-Control"
)

// Codec is used to encode/decode query range requests and responses so they can be passed down to middlewares.
type Codec interface {
	Merger
	// DecodeRequest decodes a Request from an http request.
	DecodeRequest(context.Context, *http.Request) (Request, error)
	// DecodeResponse decodes a Response from an http response.
	// The original request is also passed as a parameter this is useful for implementation that needs the request
	// to merge result or build the result correctly.
	DecodeResponse(context.Context, *http.Response, Request, log.Logger) (Response, error)
	// EncodeRequest encodes a Request into an http request.
	EncodeRequest(context.Context, Request) (*http.Request, error)
	// EncodeResponse encodes a Response into an http response.
	EncodeResponse(context.Context, Response) (*http.Response, error)
}

// Merger is used by middlewares making multiple requests to merge back all responses into a single one.
type Merger interface {
	// MergeResponse merges responses from multiple requests into a single Response
	MergeResponse(...Response) (Response, error)
}

// Request represents a query range request that can be process by middlewares.
type Request interface {
	// GetId returns the ID of the request used by splitAndCacheMiddleware to correlate downstream requests and responses.
	GetId() int64
	// GetStart returns the start timestamp of the request in milliseconds.
	GetStart() int64
	// GetEnd returns the end timestamp of the request in milliseconds.
	GetEnd() int64
	// GetStep returns the step of the request in milliseconds.
	GetStep() int64
	// GetQuery returns the query of the request.
	GetQuery() string
	// GetOptions returns the options for the given request.
	GetOptions() Options
	// GetHints returns hints that could be optionally attached to the request to pass down the stack.
	// These hints can be used to optimize the query execution.
	GetHints() *Hints
	// WithID clones the current request with the provided ID.
	WithID(id int64) Request
	// WithStartEnd clone the current request with different start and end timestamp.
	WithStartEnd(startTime int64, endTime int64) Request
	// WithQuery clone the current request with a different query.
	WithQuery(string) Request
	// WithHints clone the current request with the provided hints.
	WithHints(hints *Hints) Request
	proto.Message
	// LogToSpan writes information about this request to an OpenTracing span
	LogToSpan(opentracing.Span)
}

// Response represents a query range response.
type Response interface {
	proto.Message
	// GetHeaders returns the HTTP headers in the response.
	GetHeaders() []*PrometheusResponseHeader
}

type prometheusCodec struct{}

// WithID clones the current `PrometheusRangeQueryRequest` with the provided ID.
func (q *PrometheusRangeQueryRequest) WithID(id int64) Request {
	new := *q
	new.Id = id
	return &new
}

// WithStartEnd clones the current `PrometheusRangeQueryRequest` with a new `start` and `end` timestamp.
func (q *PrometheusRangeQueryRequest) WithStartEnd(start int64, end int64) Request {
	new := *q
	new.Start = start
	new.End = end
	return &new
}

// WithQuery clones the current `PrometheusRangeQueryRequest` with a new query.
func (q *PrometheusRangeQueryRequest) WithQuery(query string) Request {
	new := *q
	new.Query = query
	return &new
}

// WithQuery clones the current `PrometheusRangeQueryRequest` with new hints.
func (q *PrometheusRangeQueryRequest) WithHints(hints *Hints) Request {
	new := *q
	new.Hints = hints
	return &new
}

// LogToSpan logs the current `PrometheusRangeQueryRequest` parameters to the specified span.
func (q *PrometheusRangeQueryRequest) LogToSpan(sp opentracing.Span) {
	sp.LogFields(
		otlog.String("query", q.GetQuery()),
		otlog.String("start", timestamp.Time(q.GetStart()).String()),
		otlog.String("end", timestamp.Time(q.GetEnd()).String()),
		otlog.Int64("step (ms)", q.GetStep()),
	)
}

func (r *PrometheusInstantQueryRequest) GetStart() int64 {
	return r.GetTime()
}

func (r *PrometheusInstantQueryRequest) GetEnd() int64 {
	return r.GetTime()
}

func (r *PrometheusInstantQueryRequest) GetStep() int64 {
	return 0
}

func (r *PrometheusInstantQueryRequest) WithID(id int64) Request {
	new := *r
	new.Id = id
	return &new
}

func (r *PrometheusInstantQueryRequest) WithStartEnd(startTime int64, endTime int64) Request {
	new := *r
	new.Time = startTime
	return &new
}

func (r *PrometheusInstantQueryRequest) WithQuery(s string) Request {
	new := *r
	new.Query = s
	return &new
}

func (r *PrometheusInstantQueryRequest) WithHints(hints *Hints) Request {
	new := *r
	new.Hints = hints
	return &new
}

func (r *PrometheusInstantQueryRequest) LogToSpan(sp opentracing.Span) {
	sp.LogFields(
		otlog.String("query", r.GetQuery()),
		otlog.String("time", timestamp.Time(r.GetTime()).String()),
	)
}

type byFirstTime []*PrometheusResponse

func (a byFirstTime) Len() int           { return len(a) }
func (a byFirstTime) Swap(i, j int)      { a[i], a[j] = a[j], a[i] }
func (a byFirstTime) Less(i, j int) bool { return a[i].minTime() < a[j].minTime() }

func (resp *PrometheusResponse) minTime() int64 {
	result := resp.Data.Result
	if len(result) == 0 {
		return -1
	}
	if len(result[0].Samples) == 0 {
		return -1
	}
	return result[0].Samples[0].TimestampMs
}

// NewEmptyPrometheusResponse returns an empty successful Prometheus query range response.
func NewEmptyPrometheusResponse() *PrometheusResponse {
	return &PrometheusResponse{
		Status: StatusSuccess,
		Data: &PrometheusData{
			ResultType: model.ValMatrix.String(),
			Result:     []SampleStream{},
		},
	}
}

func (prometheusCodec) MergeResponse(responses ...Response) (Response, error) {
	if len(responses) == 0 {
		return NewEmptyPrometheusResponse(), nil
	}

	promResponses := make([]*PrometheusResponse, 0, len(responses))
	// we need to pass on all the headers for results cache gen numbers.
	var resultsCacheGenNumberHeaderValues []string

	for _, res := range responses {
		pr := res.(*PrometheusResponse)
		if pr.Status != StatusSuccess {
			return nil, fmt.Errorf("can't merge an unsuccessful response")
		} else if pr.Data == nil {
			return nil, fmt.Errorf("can't merge response with no data")
		} else if pr.Data.ResultType != model.ValMatrix.String() {
			return nil, fmt.Errorf("can't merge result type %q", pr.Data.ResultType)
		}

		promResponses = append(promResponses, pr)
		resultsCacheGenNumberHeaderValues = append(resultsCacheGenNumberHeaderValues, getHeaderValuesWithName(res, ResultsCacheGenNumberHeaderName)...)
	}

	// Merge the responses.
	sort.Sort(byFirstTime(promResponses))

	response := PrometheusResponse{
		Status: StatusSuccess,
		Data: &PrometheusData{
			ResultType: model.ValMatrix.String(),
			Result:     matrixMerge(promResponses),
		},
	}

	if len(resultsCacheGenNumberHeaderValues) != 0 {
		response.Headers = []*PrometheusResponseHeader{{
			Name:   ResultsCacheGenNumberHeaderName,
			Values: resultsCacheGenNumberHeaderValues,
		}}
	}

	return &response, nil
}

func (c prometheusCodec) DecodeRequest(_ context.Context, r *http.Request) (Request, error) {
	switch {
	case isRangeQuery(r.URL.Path):
		return c.decodeRangeQueryRequest(r)
	case isInstantQuery(r.URL.Path):
		return c.decodeInstantQueryRequest(r)
	default:
		return nil, fmt.Errorf("prometheus codec doesn't support requests to %s", r.URL.Path)
	}

}

func (c prometheusCodec) decodeRangeQueryRequest(r *http.Request) (Request, error) {
	var result PrometheusRangeQueryRequest
	var err error
	result.Start, err = util.ParseTime(r.FormValue("start"))
	if err != nil {
		return nil, decorateWithParamName(err, "start")
	}

	result.End, err = util.ParseTime(r.FormValue("end"))
	if err != nil {
		return nil, decorateWithParamName(err, "end")
	}

	if result.End < result.Start {
		return nil, errEndBeforeStart
	}

	result.Step, err = parseDurationMs(r.FormValue("step"))
	if err != nil {
		return nil, decorateWithParamName(err, "step")
	}

	if result.Step <= 0 {
		return nil, errNegativeStep
	}

	// For safety, limit the number of returned points per timeseries.
	// This is sufficient for 60s resolution for a week or 1h resolution for a year.
	if (result.End-result.Start)/result.Step > 11000 {
		return nil, errStepTooSmall
	}

	result.Query = r.FormValue("query")
	result.Path = r.URL.Path
	DecodeOptions(r, &result.Options)
	return &result, nil
}

func (c prometheusCodec) decodeInstantQueryRequest(r *http.Request) (Request, error) {
	var result PrometheusInstantQueryRequest
	var err error
	result.Time, err = util.ParseTime(r.FormValue("time"))
	if err != nil {
		return nil, decorateWithParamName(err, "time")
	}

	result.Query = r.FormValue("query")
	result.Path = r.URL.Path
	DecodeOptions(r, &result.Options)
	return &result, nil
}

func (prometheusCodec) EncodeRequest(ctx context.Context, r Request) (*http.Request, error) {
	var u *url.URL
	switch r := r.(type) {
	case *PrometheusRangeQueryRequest:
		u = &url.URL{
			Path: r.Path,
			RawQuery: url.Values{
				"start": []string{encodeTime(r.Start)},
				"end":   []string{encodeTime(r.End)},
				"step":  []string{encodeDurationMs(r.Step)},
				"query": []string{r.Query},
			}.Encode(),
		}
	case *PrometheusInstantQueryRequest:
		u = &url.URL{
			Path: r.Path,
			RawQuery: url.Values{
				"time":  []string{encodeTime(r.Time)},
				"query": []string{r.Query},
			}.Encode(),
		}
	default:
		return nil, fmt.Errorf("unsupported request type %T", r)
	}

	req := &http.Request{
		Method:     "GET",
		RequestURI: u.String(), // This is what the httpgrpc code looks at.
		URL:        u,
		Body:       http.NoBody,
		Header:     http.Header{},
	}

	return req.WithContext(ctx), nil
}

func (prometheusCodec) DecodeResponse(ctx context.Context, r *http.Response, _ Request, logger log.Logger) (Response, error) {
	if r.StatusCode/100 != 2 {
		body, _ := ioutil.ReadAll(r.Body)
		return nil, httpgrpc.ErrorFromHTTPResponse(&httpgrpc.HTTPResponse{
			Code: int32(r.StatusCode),
			Body: body,
		})
	}
	log, ctx := spanlogger.NewWithLogger(ctx, logger, "ParseQueryRangeResponse") //nolint:ineffassign,staticcheck
	defer log.Finish()

	buf, err := bodyBuffer(r)
	if err != nil {
		log.Error(err)
		return nil, err
	}
	log.LogFields(otlog.Int("bytes", len(buf)))

	var resp PrometheusResponse
	if err := json.Unmarshal(buf, &resp); err != nil {
		return nil, apierror.Newf(apierror.TypeInternal, "error decoding response: %v", err)
	}

	for h, hv := range r.Header {
		resp.Headers = append(resp.Headers, &PrometheusResponseHeader{Name: h, Values: hv})
	}
	return &resp, nil
}

func (d *PrometheusData) UnmarshalJSON(b []byte) error {
	v := struct {
		Type   model.ValueType    `json:"resultType"`
		Result stdjson.RawMessage `json:"result"`
	}{}

	err := json.Unmarshal(b, &v)
	if err != nil {
		return err
	}
	d.ResultType = v.Type.String()
	switch v.Type {
	case model.ValString:
		var sss stringSampleStreams
		if err := json.Unmarshal(v.Result, &sss); err != nil {
			return err
		}
		d.Result = sss
		return nil

	case model.ValScalar:
		var sss scalarSampleStreams
		if err := json.Unmarshal(v.Result, &sss); err != nil {
			return err
		}
		d.Result = sss
		return nil

	case model.ValVector:
		var vss []vectorSampleStream
		if err := json.Unmarshal(v.Result, &vss); err != nil {
			return err
		}
		d.Result = fromVectorSampleStreams(vss)
		return nil

	case model.ValMatrix:
		return json.Unmarshal(v.Result, &d.Result)

	default:
		return fmt.Errorf("unsupported value type %q", v.Type)
	}
}

// Buffer can be used to read a response body.
// This allows to avoid reading the body multiple times from the `http.Response.Body`.
type Buffer interface {
	Bytes() []byte
}

func bodyBuffer(res *http.Response) ([]byte, error) {
	// Attempt to cast the response body to a Buffer and use it if possible.
	// This is because the frontend may have already read the body and buffered it.
	if buffer, ok := res.Body.(Buffer); ok {
		return buffer.Bytes(), nil
	}
	// Preallocate the buffer with the exact size so we don't waste allocations
	// while progressively growing an initial small buffer. The buffer capacity
	// is increased by MinRead to avoid extra allocations due to how ReadFrom()
	// internally works.
	buf := bytes.NewBuffer(make([]byte, 0, res.ContentLength+bytes.MinRead))
	if _, err := buf.ReadFrom(res.Body); err != nil {
		return nil, apierror.Newf(apierror.TypeInternal, "error decoding response: %v", err)
	}
	return buf.Bytes(), nil
}

func (prometheusCodec) EncodeResponse(ctx context.Context, res Response) (*http.Response, error) {
	sp, _ := opentracing.StartSpanFromContext(ctx, "APIResponse.ToHTTPResponse")
	defer sp.Finish()

	a, ok := res.(*PrometheusResponse)
	if !ok {
		return nil, apierror.Newf(apierror.TypeInternal, "invalid response format")
	}
	if a.Data != nil {
		sp.LogFields(otlog.Int("series", len(a.Data.Result)))
	}

	b, err := json.Marshal(a)
	if err != nil {
		return nil, apierror.Newf(apierror.TypeInternal, "error encoding response: %v", err)
	}

	sp.LogFields(otlog.Int("bytes", len(b)))

	resp := http.Response{
		Header: http.Header{
			"Content-Type": []string{"application/json"},
		},
		Body:          ioutil.NopCloser(bytes.NewBuffer(b)),
		StatusCode:    http.StatusOK,
		ContentLength: int64(len(b)),
	}
	return &resp, nil
}

func (d *PrometheusData) MarshalJSON() ([]byte, error) {
	if d == nil {
		return []byte("null"), nil
	}

	switch d.ResultType {
	case model.ValString.String():
		return json.Marshal(struct {
			Type   model.ValueType     `json:"resultType"`
			Result stringSampleStreams `json:"result"`
		}{
			Type:   model.ValString,
			Result: d.Result,
		})

	case model.ValScalar.String():
		return json.Marshal(struct {
			Type   model.ValueType     `json:"resultType"`
			Result scalarSampleStreams `json:"result"`
		}{
			Type:   model.ValScalar,
			Result: d.Result,
		})

	case model.ValVector.String():
		return json.Marshal(struct {
			Type   model.ValueType      `json:"resultType"`
			Result []vectorSampleStream `json:"result"`
		}{
			Type:   model.ValVector,
			Result: asVectorSampleStreams(d.Result),
		})

	case model.ValMatrix.String():
		type plain *PrometheusData
		return json.Marshal(plain(d))

	default:
		return nil, fmt.Errorf("can't marshal prometheus result type %q", d.ResultType)
	}
}

type stringSampleStreams []SampleStream

func (sss stringSampleStreams) MarshalJSON() ([]byte, error) {
	if len(sss) != 1 {
		return nil, fmt.Errorf("string sample streams should have exactly one stream, got %d", len(sss))
	}
	ss := sss[0]
	if len(ss.Labels) != 1 || ss.Labels[0].Name != "value" {
		return nil, fmt.Errorf("string sample stream should have exactly one label called value, got %d: %v", len(ss.Labels), ss.Labels)
	}
	l := ss.Labels[0]

	if len(ss.Samples) != 1 {
		return nil, fmt.Errorf("string sample stream should have exactly one sample, got %d", len(ss.Samples))
	}
	s := ss.Samples[0]

	return json.Marshal(model.String{Value: l.Value, Timestamp: model.Time(s.TimestampMs)})
}

func (sss *stringSampleStreams) UnmarshalJSON(b []byte) error {
	var sv model.String
	if err := json.Unmarshal(b, &sv); err != nil {
		return err
	}
	*sss = []SampleStream{{
		Labels:  []mimirpb.LabelAdapter{{Name: "value", Value: sv.Value}},
		Samples: []mimirpb.Sample{{TimestampMs: int64(sv.Timestamp)}},
	}}
	return nil
}

type scalarSampleStreams []SampleStream

func (sss scalarSampleStreams) MarshalJSON() ([]byte, error) {
	if len(sss) != 1 {
		return nil, fmt.Errorf("scalar sample streams should have exactly one stream, got %d", len(sss))
	}
	ss := sss[0]
	if len(ss.Samples) != 1 {
		return nil, fmt.Errorf("scalar sample stream should have exactly one sample, got %d", len(ss.Samples))
	}
	s := ss.Samples[0]
	return json.Marshal(model.Scalar{
		Timestamp: model.Time(s.TimestampMs),
		Value:     model.SampleValue(s.Value),
	})
}

func (sss *scalarSampleStreams) UnmarshalJSON(b []byte) error {
	var sv model.Scalar
	if err := json.Unmarshal(b, &sv); err != nil {
		return err
	}
	*sss = []SampleStream{{
		Samples: []mimirpb.Sample{{TimestampMs: int64(sv.Timestamp), Value: float64(sv.Value)}},
	}}
	return nil
}

// asVectorSampleStreams converts a slice of SampleStream into a slice of vectorSampleStream.
// This can be done as vectorSampleStream is defined as a SampleStream.
func asVectorSampleStreams(ss []SampleStream) []vectorSampleStream {
	return *(*[]vectorSampleStream)(unsafe.Pointer(&ss))
}

// fromVectorSampleStreams is the inverse of asVectorSampleStreams.
func fromVectorSampleStreams(vss []vectorSampleStream) []SampleStream {
	return *(*[]SampleStream)(unsafe.Pointer(&vss))
}

type vectorSampleStream SampleStream

func (vs *vectorSampleStream) UnmarshalJSON(b []byte) error {
	s := model.Sample{}
	if err := json.Unmarshal(b, &s); err != nil {
		return err
	}
	*vs = vectorSampleStream{
		Labels:  mimirpb.FromMetricsToLabelAdapters(s.Metric),
		Samples: []mimirpb.Sample{{TimestampMs: int64(s.Timestamp), Value: float64(s.Value)}},
	}
	return nil
}

func (vs vectorSampleStream) MarshalJSON() ([]byte, error) {
	if len(vs.Samples) != 1 {
		return nil, fmt.Errorf("vector sample stream should have exactly one sample, got %d", len(vs.Samples))
	}
	return json.Marshal(model.Sample{
		Metric:    mimirpb.FromLabelAdaptersToMetric(vs.Labels),
		Timestamp: model.Time(vs.Samples[0].TimestampMs),
		Value:     model.SampleValue(vs.Samples[0].Value),
	})
}

// UnmarshalJSON implements json.Unmarshaler.
func (s *SampleStream) UnmarshalJSON(data []byte) error {
	var stream struct {
		Metric model.Metric     `json:"metric"`
		Values []mimirpb.Sample `json:"values"`
	}
	if err := json.Unmarshal(data, &stream); err != nil {
		return err
	}
	s.Labels = mimirpb.FromMetricsToLabelAdapters(stream.Metric)
	s.Samples = stream.Values
	return nil
}

// MarshalJSON implements json.Marshaler.
func (s *SampleStream) MarshalJSON() ([]byte, error) {
	stream := struct {
		Metric model.Metric     `json:"metric"`
		Values []mimirpb.Sample `json:"values"`
	}{
		Metric: mimirpb.FromLabelAdaptersToMetric(s.Labels),
		Values: s.Samples,
	}
	return json.Marshal(stream)
}

func matrixMerge(resps []*PrometheusResponse) []SampleStream {
	output := map[string]*SampleStream{}
	for _, resp := range resps {
		if resp.Data == nil {
			continue
		}
		for _, stream := range resp.Data.Result {
			metric := mimirpb.FromLabelAdaptersToLabels(stream.Labels).String()
			existing, ok := output[metric]
			if !ok {
				existing = &SampleStream{
					Labels: stream.Labels,
				}
			}
			// We need to make sure we don't repeat samples. This causes some visualisations to be broken in Grafana.
			// The prometheus API is inclusive of start and end timestamps.
			if len(existing.Samples) > 0 && len(stream.Samples) > 0 {
				existingEndTs := existing.Samples[len(existing.Samples)-1].TimestampMs
				if existingEndTs == stream.Samples[0].TimestampMs {
					// Typically this the cases where only 1 sample point overlap,
					// so optimize with simple code.
					stream.Samples = stream.Samples[1:]
				} else if existingEndTs > stream.Samples[0].TimestampMs {
					// Overlap might be big, use heavier algorithm to remove overlap.
					stream.Samples = sliceSamples(stream.Samples, existingEndTs)
				} // else there is no overlap, yay!
			}
			existing.Samples = append(existing.Samples, stream.Samples...)
			output[metric] = existing
		}
	}

	keys := make([]string, 0, len(output))
	for key := range output {
		keys = append(keys, key)
	}
	sort.Strings(keys)

	result := make([]SampleStream, 0, len(output))
	for _, key := range keys {
		result = append(result, *output[key])
	}

	return result
}

// sliceSamples assumes given samples are sorted by timestamp in ascending order and
// return a sub slice whose first element's is the smallest timestamp that is strictly
// bigger than the given minTs. Empty slice is returned if minTs is bigger than all the
// timestamps in samples.
func sliceSamples(samples []mimirpb.Sample, minTs int64) []mimirpb.Sample {
	if len(samples) <= 0 || minTs < samples[0].TimestampMs {
		return samples
	}

	if len(samples) > 0 && minTs > samples[len(samples)-1].TimestampMs {
		return samples[len(samples):]
	}

	searchResult := sort.Search(len(samples), func(i int) bool {
		return samples[i].TimestampMs > minTs
	})

	return samples[searchResult:]
}

func parseDurationMs(s string) (int64, error) {
	if d, err := strconv.ParseFloat(s, 64); err == nil {
		ts := d * float64(time.Second/time.Millisecond)
		if ts > float64(math.MaxInt64) || ts < float64(math.MinInt64) {
			return 0, apierror.Newf(apierror.TypeBadData, "cannot parse %q to a valid duration. It overflows int64", s)
		}
		return int64(ts), nil
	}
	if d, err := model.ParseDuration(s); err == nil {
		return int64(d) / int64(time.Millisecond/time.Nanosecond), nil
	}
	return 0, apierror.Newf(apierror.TypeBadData, "cannot parse %q to a valid duration", s)
}

func encodeTime(t int64) string {
	f := float64(t) / 1.0e3
	return strconv.FormatFloat(f, 'f', -1, 64)
}

func encodeDurationMs(d int64) string {
	return strconv.FormatFloat(float64(d)/float64(time.Second/time.Millisecond), 'f', -1, 64)
}

func decorateWithParamName(err error, field string) error {
	errTmpl := "invalid parameter %q: %v"
	if status, ok := status.FromError(err); ok {
		return apierror.Newf(apierror.TypeBadData, errTmpl, field, status.Message())
	}
	return apierror.Newf(apierror.TypeBadData, errTmpl, field, err)
}

// isRequestStepAligned returns whether the Request start and end timestamps are aligned
// with the step.
func isRequestStepAligned(req Request) bool {
	if req.GetStep() == 0 {
		return true
	}

	return req.GetEnd()%req.GetStep() == 0 && req.GetStart()%req.GetStep() == 0
}
