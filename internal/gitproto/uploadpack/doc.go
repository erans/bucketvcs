// Package uploadpack drives the Git upload-pack protocol over arbitrary
// (io.Reader, io.Writer) pairs. HTTP gateway handlers and the SSH session
// handler are both adapters around this engine.
//
// The engine has three entry points:
//
//	Advertise — write the initial ref/capability advertisement
//	Service   — read wants/haves from Stdin, write pack/responses to Stdout
//	Serve     — Advertise then Service (used by SSH; HTTP splits across
//	            info/refs and POST upload-pack handlers)
//
// The engine has no HTTP, no SSH, and no SQL imports.
package uploadpack
