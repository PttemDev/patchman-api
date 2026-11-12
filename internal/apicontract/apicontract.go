// Package apicontract declares the client-facing API contract version.
//
// It is a plain integer surfaced by GET /api/config as `apiVersion`. A desktop
// client checks it against its own supported range to decide whether this
// backend is compatible (a backend far newer than the app, or older than the
// app still supports, is refused with a clear message instead of failing in
// weird ways later).
//
// BUMP DISCIPLINE: increment Version ONLY on a breaking, client-visible API
// change — and in the SAME release widen the FE's SUPPORTED_API_MAX
// (fe/src/lib/apiCompat.ts). Backward-compatible additions do NOT bump it.
package apicontract

// Version is the current API contract version. Started at 1.
const Version = 1
