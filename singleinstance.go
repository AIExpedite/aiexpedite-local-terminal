package main

import "sync"

// instanceReleaseSlot lets a platform handoff release the process-wide
// singleton before launching its replacement. The returned wrapper remains
// safe to defer: both normal shutdown and update handoff share the same Once.
type instanceReleaseSlot struct {
	once    sync.Once
	release func()
}

var (
	instanceReleaseMu sync.Mutex
	instanceRelease   *instanceReleaseSlot
)

func trackAgentInstanceRelease(release func()) func() {
	slot := &instanceReleaseSlot{release: release}
	instanceReleaseMu.Lock()
	instanceRelease = slot
	instanceReleaseMu.Unlock()

	return func() { releaseAgentInstanceSlot(slot) }
}

func releaseAgentInstanceSlot(slot *instanceReleaseSlot) {
	if slot == nil {
		return
	}
	slot.once.Do(func() {
		if slot.release != nil {
			slot.release()
		}
		instanceReleaseMu.Lock()
		if instanceRelease == slot {
			instanceRelease = nil
		}
		instanceReleaseMu.Unlock()
	})
}

// releaseAgentInstanceForHandoff releases the old process's singleton before
// a replacement is launched, so the replacement cannot observe contention and
// exit while the old process is also committed to quitting.
func releaseAgentInstanceForHandoff() {
	instanceReleaseMu.Lock()
	slot := instanceRelease
	instanceReleaseMu.Unlock()
	releaseAgentInstanceSlot(slot)
}
