//go:build !windows

package snap

// The change journal shortcut is NTFS only; elsewhere every sync walks.
type usnState struct{}

func (m *Mirror) usnStart() (usnState, bool) { return usnState{}, false }
func (m *Mirror) usnSave(usnState)           {}
func (m *Mirror) usnClean() bool             { return false }
func (m *Mirror) usnAdvance(usnState)        {}
