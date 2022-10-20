// Unless explicitly stated otherwise all files in this repository are licensed
// under the Apache License Version 2.0.
// This product includes software developed at Datadog (https://www.datadoghq.com/).
// Copyright 2016 Datadog, Inc.

// Package httpsec defines is the HTTP instrumentation API and contract for
// AppSec. It defines an abstract representation of HTTP handlers, along with
// helper functions to wrap (aka. instrument) standard net/http handlers.
// HTTP integrations must use this package to enable AppSec features for HTTP,
// which listens to this package's operation events.
package httpsec

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"reflect"
	"strings"

	"gopkg.in/DataDog/dd-trace-go.v1/ddtrace"
	"gopkg.in/DataDog/dd-trace-go.v1/internal/appsec/dyngo"
	"gopkg.in/DataDog/dd-trace-go.v1/internal/appsec/dyngo/instrumentation"
	"gopkg.in/DataDog/dd-trace-go.v1/internal/log"
)

// Abstract HTTP handler operation definition.
type (
	// HandlerOperationArgs is the HTTP handler operation arguments.
	HandlerOperationArgs struct {
		// RequestURI corresponds to the address `server.request.uri.raw`
		RequestURI string
		// Headers corresponds to the address `server.request.headers.no_cookies`
		Headers map[string][]string
		// Cookies corresponds to the address `server.request.cookies`
		Cookies map[string][]string
		// Query corresponds to the address `server.request.query`
		Query map[string][]string
		// PathParams corresponds to the address `server.request.path_params`
		PathParams map[string]string
		// ClientIP corresponds to the addres `http.client_ip`
		ClientIP netaddrIP
	}

	// HandlerOperationRes is the HTTP handler operation results.
	HandlerOperationRes struct {
		// Status corresponds to the address `server.response.status`.
		Status int
	}

	// SDKBodyOperationArgs is the SDK body operation arguments.
	SDKBodyOperationArgs struct {
		// Body corresponds to the address `server.request.body`.
		Body interface{}
	}

	// SDKBodyOperationRes is the SDK body operation results.
	SDKBodyOperationRes struct{}
)

// MonitorParsedBody starts and finishes the SDK body operation.
// This function should not be called when AppSec is disabled in order to
// get preciser error logs.
func MonitorParsedBody(ctx context.Context, body interface{}) {
	if parent := fromContext(ctx); parent != nil {
		op := StartSDKBodyOperation(parent, SDKBodyOperationArgs{Body: body})
		op.Finish()
	} else {
		log.Error("appsec: parsed http body monitoring ignored: could not find the http handler instrumentation metadata in the request context: the request handler is not being monitored by a middleware function or the provided context is not the expected request context")
	}
}

// WrapHandler wraps the given HTTP handler with the abstract HTTP operation defined by HandlerOperationArgs and
// HandlerOperationRes.
func WrapHandler(handler http.Handler, span ddtrace.Span, pathParams map[string]string) http.Handler {
	instrumentation.SetAppSecEnabledTags(span)

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		SetIPTags(span, r)

		args := MakeHandlerOperationArgs(r, pathParams)
		ctx, op := StartOperation(r.Context(), args)
		r = r.WithContext(ctx)
		defer func() {
			var status int
			if mw, ok := w.(interface{ Status() int }); ok {
				status = mw.Status()
			}

			events := op.Finish(HandlerOperationRes{Status: status})
			instrumentation.SetTags(span, op.Tags())
			if len(events) == 0 {
				return
			}

			remoteIP, _, err := net.SplitHostPort(r.RemoteAddr)
			if err != nil {
				remoteIP = r.RemoteAddr
			}
			SetSecurityEventTags(span, events, remoteIP, args.Headers, w.Header())
		}()

		handler.ServeHTTP(w, r)
	})
}

// MakeHandlerOperationArgs creates the HandlerOperationArgs out of a standard
// http.Request along with the given current span. It returns an empty structure
// when appsec is disabled.
func MakeHandlerOperationArgs(r *http.Request, pathParams map[string]string) HandlerOperationArgs {
	headers := make(http.Header, len(r.Header))
	for k, v := range r.Header {
		k := strings.ToLower(k)
		if k == "cookie" {
			// Do not include cookies in the request headers
			continue
		}
		headers[k] = v
	}
	cookies := makeCookies(r) // TODO(Julio-Guerra): avoid actively parsing the cookies thanks to dynamic instrumentation
	headers["host"] = []string{r.Host}
	return HandlerOperationArgs{
		RequestURI: r.RequestURI,
		Headers:    headers,
		Cookies:    cookies,
		Query:      r.URL.Query(), // TODO(Julio-Guerra): avoid actively parsing the query values thanks to dynamic instrumentation
		PathParams: pathParams,
		ClientIP:   IPFromRequest(r),
	}
}

// Return the map of parsed cookies if any and following the specification of
// the rule address `server.request.cookies`.
func makeCookies(r *http.Request) map[string][]string {
	parsed := r.Cookies()
	if len(parsed) == 0 {
		return nil
	}
	cookies := make(map[string][]string, len(parsed))
	for _, c := range parsed {
		cookies[c.Name] = append(cookies[c.Name], c.Value)
	}
	return cookies
}

// TODO(Julio-Guerra): create a go-generate tool to generate the types, vars and methods below

// Operation type representing an HTTP operation. It must be created with
// StartOperation() and finished with its Finish().
type (
	Operation struct {
		dyngo.Operation
		instrumentation.TagsHolder
		instrumentation.SecurityEventsHolder
	}

	// SDKBodyOperation type representing an SDK body. It must be created with
	// StartSDKBodyOperation() and finished with its Finish() method.
	SDKBodyOperation struct {
		dyngo.Operation
	}

	contextKey struct{}
)

// StartOperation starts an HTTP handler operation, along with the given
// context and arguments and emits a start event up in the operation stack.
// The operation is linked to the global root operation since an HTTP operation
// is always expected to be first in the operation stack.
func StartOperation(ctx context.Context, args HandlerOperationArgs) (context.Context, *Operation) {
	op := &Operation{
		Operation:  dyngo.NewOperation(nil),
		TagsHolder: instrumentation.NewTagsHolder(),
	}
	newCtx := context.WithValue(ctx, contextKey{}, op)
	dyngo.StartOperation(op, args)
	return newCtx, op
}

func fromContext(ctx context.Context) *Operation {
	// Avoid a runtime panic in case of type-assertion error by collecting the 2 return values
	op, _ := ctx.Value(contextKey{}).(*Operation)
	return op
}

// Finish the HTTP handler operation, along with the given results and emits a
// finish event up in the operation stack.
func (op *Operation) Finish(res HandlerOperationRes) []json.RawMessage {
	dyngo.FinishOperation(op, res)
	return op.Events()
}

// StartSDKBodyOperation starts the SDKBody operation and emits a start event
func StartSDKBodyOperation(parent *Operation, args SDKBodyOperationArgs) *SDKBodyOperation {
	op := &SDKBodyOperation{Operation: dyngo.NewOperation(parent)}
	dyngo.StartOperation(op, args)
	return op
}

// Finish finishes the SDKBody operation and emits a finish event
func (op *SDKBodyOperation) Finish() {
	dyngo.FinishOperation(op, SDKBodyOperationRes{})
}

// HTTP handler operation's start and finish event callback function types.
type (
	// OnHandlerOperationStart function type, called when an HTTP handler
	// operation starts.
	OnHandlerOperationStart func(*Operation, HandlerOperationArgs)
	// OnHandlerOperationFinish function type, called when an HTTP handler
	// operation finishes.
	OnHandlerOperationFinish func(*Operation, HandlerOperationRes)
	// OnSDKBodyOperationStart function type, called when an SDK body
	// operation starts.
	OnSDKBodyOperationStart func(*SDKBodyOperation, SDKBodyOperationArgs)
	// OnSDKBodyOperationFinish function type, called when an SDK body
	// operation finishes.
	OnSDKBodyOperationFinish func(*SDKBodyOperation, SDKBodyOperationRes)
)

var (
	handlerOperationArgsType = reflect.TypeOf((*HandlerOperationArgs)(nil)).Elem()
	handlerOperationResType  = reflect.TypeOf((*HandlerOperationRes)(nil)).Elem()
	sdkBodyOperationArgsType = reflect.TypeOf((*SDKBodyOperationArgs)(nil)).Elem()
	sdkBodyOperationResType  = reflect.TypeOf((*SDKBodyOperationRes)(nil)).Elem()
)

// ListenedType returns the type a OnHandlerOperationStart event listener
// listens to, which is the HandlerOperationArgs type.
func (OnHandlerOperationStart) ListenedType() reflect.Type { return handlerOperationArgsType }

// Call calls the underlying event listener function by performing the
// type-assertion on v whose type is the one returned by ListenedType().
func (f OnHandlerOperationStart) Call(op dyngo.Operation, v interface{}) {
	f(op.(*Operation), v.(HandlerOperationArgs))
}

// ListenedType returns the type a OnHandlerOperationFinish event listener
// listens to, which is the HandlerOperationRes type.
func (OnHandlerOperationFinish) ListenedType() reflect.Type { return handlerOperationResType }

// Call calls the underlying event listener function by performing the
// type-assertion on v whose type is the one returned by ListenedType().
func (f OnHandlerOperationFinish) Call(op dyngo.Operation, v interface{}) {
	f(op.(*Operation), v.(HandlerOperationRes))
}

// ListenedType returns the type a OnSDKBodyOperationStart event listener
// listens to, which is the SDKBodyOperationStartArgs type.
func (OnSDKBodyOperationStart) ListenedType() reflect.Type { return sdkBodyOperationArgsType }

// Call calls the underlying event listener function by performing the
// type-assertion  on v whose type is the one returned by ListenedType().
func (f OnSDKBodyOperationStart) Call(op dyngo.Operation, v interface{}) {
	f(op.(*SDKBodyOperation), v.(SDKBodyOperationArgs))
}

// ListenedType returns the type a OnSDKBodyOperationFinish event listener
// listens to, which is the SDKBodyOperationRes type.
func (OnSDKBodyOperationFinish) ListenedType() reflect.Type { return sdkBodyOperationResType }

// Call calls the underlying event listener function by performing the
// type-assertion on v whose type is the one returned by ListenedType().
func (f OnSDKBodyOperationFinish) Call(op dyngo.Operation, v interface{}) {
	f(op.(*SDKBodyOperation), v.(SDKBodyOperationRes))
}

// IPFromRequest returns the resolved client IP for a specific request. The returned IP can be invalid.
func IPFromRequest(r *http.Request) netaddrIP {
	ipHeaders := defaultIPHeaders
	if len(clientIPHeader) > 0 {
		ipHeaders = []string{clientIPHeader}
	}
	var headers []string
	var ips []string
	for _, hdr := range ipHeaders {
		if v := r.Header.Get(hdr); v != "" {
			headers = append(headers, hdr)
			ips = append(ips, v)
		}
	}
	if len(ips) == 0 {
		if remoteIP := parseIP(r.RemoteAddr); remoteIP.IsValid() && isGlobal(remoteIP) {
			return remoteIP
		}
	} else if len(ips) == 1 {
		for _, ipstr := range strings.Split(ips[0], ",") {
			ip := parseIP(strings.TrimSpace(ipstr))
			if ip.IsValid() && isGlobal(ip) {
				return ip
			}
		}
	}

	return netaddrIP{}
}
