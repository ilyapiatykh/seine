package spec

// ApplyDefaults fills in zero-valued fields with the project defaults. It
// is idempotent and is invoked from Parse before Validate.
func (d *Document) ApplyDefaults() {
	if d.APIVersion == "" {
		d.APIVersion = APIVersion
	}
	if d.Kind == "" {
		d.Kind = Kind
	}
	wg := &d.Spec.WireGuard
	if wg.MTU == 0 {
		wg.MTU = DefaultMTU
	}
	if wg.PersistentKeepalive == 0 {
		wg.PersistentKeepalive = Duration(DefaultPersistentKeepalive)
	}
	if wg.HubListenPort == 0 {
		wg.HubListenPort = DefaultHubListenPort
	}
}
