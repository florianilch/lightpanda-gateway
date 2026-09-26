// Package httpfrontend implements lpgw's HTTP frontend. It authenticates API clients,
// validates HTTP input, serves script executions, and proxies CDP sessions over WebSockets
// through Gateway.
//
// Authentication and HTTP validation happen before Gateway admission. Requests rejected
// at those stages do not enter the Gateway queue or consume a Gateway slot. The HTTP request
// can remain open while its Gateway Begin call waits in the Gateway queue. It does not consume
// an HTTP frontend client-operation slot during that wait; the frontend takes that slot only
// after Gateway admission.
//
// Gateway and HTTP frontend capacities are independent. After Gateway admission, the
// frontend starts an HTTP client operation only when an HTTP frontend slot is immediately
// available. If no HTTP frontend slot is available, it closes the Gateway resource and
// returns 503. Otherwise, the handler closes the Gateway resource when the HTTP client
// operation ends.
//
// Gateway returns a Gateway slot only after the Gateway resource reports completion. The
// frontend returns an HTTP frontend slot after response delivery and final logging. For a
// CDP tunnel, the HTTP frontend returns the slot after the tunnel closes and its closure is
// logged. During Server.Shutdown, the HTTP frontend waits for established CDP tunnels to
// close or for the shutdown context to expire.
//
// Concurrency and Gateway queue settings do not set HTTP connection, request-rate, or OS
// resource limits.
package httpfrontend
