// Package gateway admits and tracks CDPBrowser and ScriptCall Gateway resources.
//
// BeginCDPBrowser and BeginScriptCall validate their arguments before requesting a
// Gateway slot. Their contexts control only getting that slot. After Gateway admission,
// canceling a context does not stop the returned Gateway resource. The caller must close it.
// Gateway may ask it to stop when its resource lifetime expires or shutdown begins.
//
// Contexts passed to Start, Attach, and Run can interrupt those calls. Stopping the
// Gateway resource can interrupt them as well.
//
// CDPBrowser.Start starts the Lightpanda browser process and waits until it is ready
// for Attach. Its context controls startup only. After Start succeeds, canceling the
// context does not stop the browser.
//
// CDPBrowser.Attach opens one backend connection to the browser and returns a
// CDPAttachment that owns it. Its context controls opening the connection only. After
// Attach succeeds, canceling the context does not stop the attachment. The caller must
// close each attachment when finished. Stopping the CDPBrowser may close it first.
//
// CDPAttachment.Done closes after the backend connection is closed and the CDPBrowser
// removes the attachment. CDPAttachment.Err returns the cause recorded when closing
// starts. It is nil while the attachment is open and after a normal Close.
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
// For a CDPBrowser, Done closes after Start and Attach calls return and every
// CDPAttachment closes. The CDPBrowser then waits for the browser process to exit,
// process cleanup to finish, and final logging to complete.
//
// Gateway waits for each Gateway resource's Done channel in a separate goroutine before
// releasing its slot. Close or Wait can return first, while the slot is still held.
package gateway
