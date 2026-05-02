package spec

// FindHub returns the hub with the given name, or nil if it does not exist.
// Callers should not mutate the returned pointer; specs are immutable after
// Parse.
func (d *Document) FindHub(name string) *Hub {
	for i := range d.Spec.Hubs {
		if d.Spec.Hubs[i].Name == name {
			return &d.Spec.Hubs[i]
		}
	}
	return nil
}

// FindAgent returns the agent with the given name, or nil.
func (d *Document) FindAgent(name string) *Agent {
	for i := range d.Spec.Agents {
		if d.Spec.Agents[i].Name == name {
			return &d.Spec.Agents[i]
		}
	}
	return nil
}

// AgentsForHub returns all spokes anchored to a particular hub. Useful for
// the hub itself when building its peer list.
func (d *Document) AgentsForHub(hubName string) []*Agent {
	var out []*Agent
	for i := range d.Spec.Agents {
		if d.Spec.Agents[i].Hub == hubName {
			out = append(out, &d.Spec.Agents[i])
		}
	}
	return out
}

// HasGroup reports whether name is declared in spec.groups.
func (d *Document) HasGroup(name string) bool {
	for _, g := range d.Spec.Groups {
		if g == name {
			return true
		}
	}
	return false
}
