package spec

import (
	"fmt"
	"time"
)

// Duration is a YAML-friendly time.Duration. It serialises as a Go duration
// string ("25s", "1m30s") rather than the integer-nanosecond form that
// time.Duration's default MarshalYAML would produce.
type Duration time.Duration

func (d Duration) Std() time.Duration { return time.Duration(d) }
func (d Duration) String() string     { return time.Duration(d).String() }

// UnmarshalYAML accepts both numeric (seconds) and string ("25s") forms,
// because YAML naturally writes plain numbers without quotes.
func (d *Duration) UnmarshalYAML(unmarshal func(any) error) error {
	var raw any
	if err := unmarshal(&raw); err != nil {
		return err
	}
	switch v := raw.(type) {
	case nil:
		*d = 0
		return nil
	case int:
		*d = Duration(time.Duration(v) * time.Second)
		return nil
	case int64:
		*d = Duration(time.Duration(v) * time.Second)
		return nil
	case float64:
		*d = Duration(time.Duration(v * float64(time.Second)))
		return nil
	case string:
		parsed, err := time.ParseDuration(v)
		if err != nil {
			return fmt.Errorf("parse duration %q: %w", v, err)
		}
		*d = Duration(parsed)
		return nil
	default:
		return fmt.Errorf("duration: unsupported YAML type %T", raw)
	}
}

func (d Duration) MarshalYAML() (any, error) {
	return d.String(), nil
}
