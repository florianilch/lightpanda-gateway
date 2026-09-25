// Package gateway admits and tracks ScriptCall Gateway resources.
//
// Contexts passed to Start, Attach, and Run can interrupt those calls. Stopping the
// Gateway resource can interrupt them as well.
//
// ScriptCall.Run runs at most once. If its context or the ScriptCall is canceled before
// Run decides the result, Run returns the recorded cause and no ScriptResult. Cancellation
// after that does not change the result.
//
// If the browser process starts, Run waits for it to exit and for output copying to
// finish. It then performs process cleanup and final logging before returning.
// ScriptCall.Done closes after final logging and just before Run returns.
//
// A Gateway resource's lifetime starts when Gateway admits it. When the lifetime
// expires, Gateway asks the resource to stop. The resource may still be stopping after its
// lifetime expires. It keeps its Gateway slot until Done closes.
//
// Gateway waits for each Gateway resource's Done channel in a separate goroutine before
// releasing its slot. Close or Wait can return first, while the slot is still held.
package gateway
