package wg

// New returns a platform-specific Interface implementation. The interface
// is not yet brought up — call Up to provision it.
//
// Currently only Linux is supported; on other platforms New returns an
// error so that the agent fails fast with a clear message rather than
// pretending to work.
func New(ifaceName string) (Interface, error) {
	return newPlatformInterface(ifaceName)
}
