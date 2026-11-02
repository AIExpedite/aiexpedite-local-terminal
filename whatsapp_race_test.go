//go:build race

package main

// whatsAppRaceDetector: this test binary runs under the race detector, which
// slows an outbox write by tens of times. Tests that need a write still in
// flight size it with whatsAppInFlightRecords rather than by the clock.
const whatsAppRaceDetector = true
