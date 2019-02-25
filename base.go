package jrpc2

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"bitbucket.org/creachadair/jrpc2/channel"
	"bitbucket.org/creachadair/jrpc2/code"
)

// An Assigner assigns a Handler to handle the specified method name, or nil if
// no method is available to handle the request.
type Assigner interface {
	// Assign returns the handler for the named method, or nil.
	Assign(method string) Handler

	// Names returns a slice of all known method names for the assigner.  The
	// resulting slice is ordered lexicographically and contains no duplicates.
	Names() []string
}

// A Handler handles a single request.
type Handler interface {
	// Handle invokes the method with the specified request. The response value
	// must be JSON-marshalable or nil. In case of error, the handler can
	// return a value of type *jrpc2.Error to control the response code sent
	// back to the caller; otherwise the server will wrap the resulting value.
	//
	// The context passed to the handler by a *jrpc2.Server includes two extra
	// values that the handler may extract.
	//
	// To obtain a server metrics value, write:
	//
	//    sm := jrpc2.ServerMetrics(ctx)
	//
	// To obtain the inbound request message, write:
	//
	//    req := jrpc2.InboundRequest(ctx)
	//
	// The inbound request is the same value passed to the Handle method -- the
	// latter is primarily useful in handlers generated by handler.New, which do
	// not receive this value directly.
	Handle(context.Context, *Request) (interface{}, error)
}

// A Request is a request message from a client to a server.
type Request struct {
	id     json.RawMessage // the request ID, nil for notifications
	method string          // the name of the method being requested
	params json.RawMessage // method parameters
}

// IsNotification reports whether the request is a notification, and thus does
// not require a value response.
func (r *Request) IsNotification() bool { return r.id == nil }

// ID returns the request identifier for r, or "" if r is a notification.
func (r *Request) ID() string { return string(r.id) }

// Method reports the method name for the request.
func (r *Request) Method() string { return r.method }

// HasParams reports whether the request has non-empty parameters.
func (r *Request) HasParams() bool { return len(r.params) != 0 }

// UnmarshalParams decodes the parameters into v. If r has empty parameters, it
// returns nil without modifying v.
func (r *Request) UnmarshalParams(v interface{}) error {
	if len(r.params) == 0 {
		return nil
	}
	return json.Unmarshal(r.params, v)
}

// ErrInvalidVersion is returned by ParseRequests if one or more of the
// requests in the input has a missing or invalid version marker.
var ErrInvalidVersion = Errorf(code.InvalidRequest, "incorrect version marker")

// ParseRequests parses a single request or a batch of requests from JSON.
// The result parameters are either nil or have concrete type json.RawMessage.
//
// If any of the requests is missing or has an invalid JSON-RPC version, it
// returns ErrInvalidVersion along with the parsed results. Otherwise, no
// validation apart from basic structure is performed on the results.
func ParseRequests(msg []byte) ([]*Request, error) {
	var req jrequests
	if err := json.Unmarshal(msg, &req); err != nil {
		return nil, err
	}
	var err error
	out := make([]*Request, len(req))
	for i, req := range req {
		if req.V != Version {
			err = ErrInvalidVersion
		}
		out[i] = &Request{
			id:     fixID(req.ID),
			method: req.M,
			params: req.P,
		}
	}
	return out, err
}

// A Response is a response message from a server to a client.
type Response struct {
	id     string
	err    *Error
	result json.RawMessage

	// Waiters synchronize on reading from ch. The first successful reader from
	// ch completes the request and is responsible for updating rsp and then
	// closing ch. The client owns writing to ch, and is responsible to ensure
	// that at most one write is ever performed.
	ch     chan *jresponse
	cancel func()
}

// ID returns the request identifier for r.
func (r *Response) ID() string { return r.id }

// SetID sets the request identifier for r. This is for use in proxies.
func (r *Response) SetID(id string) { r.id = id }

// Error returns a non-nil *Error if the response contains an error.
func (r *Response) Error() *Error { return r.err }

// UnmarshalResult decodes the result message into v. If the request failed,
// UnmarshalResult returns the *Error value that would also be returned by
// r.Error(), and v is unmodified.
func (r *Response) UnmarshalResult(v interface{}) error {
	if r.err != nil {
		return r.err
	}
	return json.Unmarshal(r.result, v)
}

// MarshalJSON converts the request to equivalent JSON.
func (r *Response) MarshalJSON() ([]byte, error) {
	jr := &jresponse{
		V:  Version,
		ID: json.RawMessage(r.id),
		R:  r.result,
	}
	if r.err != nil {
		jr.E = r.err.tojerror()
	}
	return json.Marshal(jr)
}

// wait blocks until p is complete. It is safe to call this multiple times and
// from concurrent goroutines.
func (r *Response) wait() {
	raw, ok := <-r.ch
	if ok {
		// N.B. We intentionally DO NOT have the sender close the channel, to
		// prevent a data race between callers of Wait. The channel is closed
		// by the first waiter to get a real value (ok == true).
		//
		// The first waiter must update the response value, THEN close the
		// channel and cancel the context. This order ensures that subsequent
		// waiters all get the same response, and do not race on accessing it.
		r.id = string(fixID(raw.ID))
		r.err = raw.E.toError()
		r.result = raw.R
		close(r.ch)
		r.cancel() // release the context observer
	}
}

// jrequests is either a single request or a slice of requests.  This handles
// the decoding of batch requests in JSON-RPC 2.0.
type jrequests []*jrequest

func (j jrequests) MarshalJSON() ([]byte, error) {
	if len(j) == 1 {
		return json.Marshal(j[0])
	}
	return json.Marshal([]*jrequest(j))
}

func (j *jrequests) UnmarshalJSON(data []byte) error {
	if len(data) == 0 {
		return errors.New("empty request message")
	} else if data[0] != '[' {
		*j = jrequests{new(jrequest)}
		return json.Unmarshal(data, (*j)[0])
	} else if err := json.Unmarshal(data, (*[]*jrequest)(j)); err != nil {
		return err
	}
	for _, req := range *j {
		req.batch = true
	}
	return nil
}

// jrequest is the transmission format of a request message.
type jrequest struct {
	V  string          `json:"jsonrpc"`      // must be Version
	ID json.RawMessage `json:"id,omitempty"` // may be nil
	M  string          `json:"method"`
	P  json.RawMessage `json:"params,omitempty"` // may be nil

	batch bool // this request was part of a batch
}

func (j *jrequest) UnmarshalJSON(data []byte) error {
	// Unmarshal into a map so we can check for extra keys.  The json.Decoder
	// has DisallowUnknownFields, but fails decoding eagerly for fields that do
	// not map to known tags. We want to fully parse the object so we can
	// propagate the "id" in error responses, if it is set. So we have to decode
	// and check the fields ourselves.

	var obj map[string]json.RawMessage
	if err := json.Unmarshal(data, &obj); err != nil {
		return Errorf(code.ParseError, "request is not a JSON object")
	}

	*j = jrequest{}    // reset
	var err error      // deferred error from parsing fields
	var extra []string // extra field names
	var id interface{} // if "id" is set, its value
	for key, val := range obj {
		switch key {
		case "jsonrpc":
			if e := json.Unmarshal(val, &j.V); e != nil && err == nil {
				err = errors.New("invalid version key")
			}
		case "id":
			id = val
			j.ID = val
		case "method":
			if e := json.Unmarshal(val, &j.M); e != nil && err == nil {
				err = errors.New("invalid method name")
			}
		case "params":
			j.P = val
		default:
			extra = append(extra, key)
		}
	}

	// Report a parse error, if we had one.
	if err != nil {
		return DataErrorf(code.ParseError, id, err.Error())
	}

	// Report an error for extraneous fields.
	if len(extra) != 0 {
		return DataErrorf(code.InvalidRequest, j.ID, "extra fields in request")
	}

	// As a special case, reduce "null" to nil in the parameters.
	if string(j.P) == "null" {
		j.P = nil
	} else if len(j.P) != 0 && j.P[0] != '[' && j.P[0] != '{' {
		return DataErrorf(code.InvalidRequest, j.ID, "parameters must be list or object")
	}
	return nil
}

// jresponses is a slice of responses, encoded as a single response if there is
// exactly one.
type jresponses []*jresponse

func (j jresponses) MarshalJSON() ([]byte, error) {
	if len(j) == 1 && !j[0].batch {
		return json.Marshal(j[0])
	}
	return json.Marshal([]*jresponse(j))
}

func (j *jresponses) UnmarshalJSON(data []byte) error {
	if len(data) == 0 {
		return errors.New("empty request message")
	} else if data[0] != '[' {
		*j = jresponses{new(jresponse)}
		return json.Unmarshal(data, (*j)[0])
	}
	return json.Unmarshal(data, (*[]*jresponse)(j))
}

// jresponse is the transmission format of a response message.
type jresponse struct {
	V  string          `json:"jsonrpc"`          // must be Version
	ID json.RawMessage `json:"id,omitempty"`     // set if request had an ID
	E  *jerror         `json:"error,omitempty"`  // set on error
	R  json.RawMessage `json:"result,omitempty"` // set on success

	// Allow the server to send a response that looks like a notification.
	// This is an extension of JSON-RPC 2.0.
	M string          `json:"method,omitempty"`
	P json.RawMessage `json:"params,omitempty"`

	batch bool // the request was part of a batch
}

func (j jresponse) isServerRequest() bool { return j.E == nil && j.R == nil && j.M != "" }

// jerror is the transmission format of an error object.
type jerror struct {
	Code int32           `json:"code"`
	Msg  string          `json:"message,omitempty"` // optional
	Data json.RawMessage `json:"data,omitempty"`    // optional
}

// toError converts a wire-format error object into an *Error.
func (e *jerror) toError() *Error {
	if e == nil {
		return nil
	}
	return &Error{
		message: e.Msg,
		code:    code.Code(e.Code),
		data:    e.Data,
	}
}

func jerrorf(code code.Code, msg string, args ...interface{}) *jerror {
	return &jerror{
		Code: int32(code),
		Msg:  fmt.Sprintf(msg, args...),
	}
}

// fixID filters id, treating "null" as a synonym for an unset ID.  This
// supports interoperation with JSON-RPC v1 where "null" is used as an ID for
// notifications.
func fixID(id json.RawMessage) json.RawMessage {
	if string(id) != "null" {
		return id
	}
	return nil
}

// encode marshals v as JSON and forwards it to the channel.
func encode(ch channel.Sender, v interface{}) (int, error) {
	bits, err := json.Marshal(v)
	if err != nil {
		return 0, err
	}
	return len(bits), ch.Send(bits)
}
