package config

import (
	"fmt"
	"log/slog"
	"time"
)

// Secret holds a sensitive value. Its String and LogValue methods redact the
// content so it never ends up in logs or error messages by accident; use
// Reveal to get the real value.
type Secret string

const redacted = "[redacted]"

func (s Secret) Reveal() string { return string(s) }

func (s Secret) String() string { return redacted }

func (s Secret) GoString() string { return redacted }

func (s Secret) LogValue() slog.Value { return slog.StringValue(redacted) }

// Duration is a time.Duration decoded from a TOML string such as "90s" or "5m".
type Duration time.Duration

func (d *Duration) UnmarshalText(text []byte) error {
	parsed, err := time.ParseDuration(string(text))
	if err != nil {
		return fmt.Errorf("invalid duration %q", text)
	}
	*d = Duration(parsed)
	return nil
}

func (d Duration) Std() time.Duration { return time.Duration(d) }

func (d Duration) String() string { return time.Duration(d).String() }
