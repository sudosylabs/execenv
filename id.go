package execenv

import "unicode/utf8"

const (
	minIDBytes = 1
	maxIDBytes = 64
)

// ValidateSpec reports ErrInvalid when id, image, or network cannot be used.
func ValidateSpec(spec Spec) error {
	if err := validateName(string(spec.ID)); err != nil {
		return err
	}
	if err := validateName(string(spec.Image)); err != nil {
		return err
	}
	switch spec.Network {
	case NetworkNone, NetworkAllowlist:
	default:
		return ErrInvalid
	}
	return nil
}

func validateName(value string) error {
	if len(value) < minIDBytes || len(value) > maxIDBytes || !utf8.ValidString(value) {
		return ErrInvalid
	}
	for i := 0; i < len(value); i++ {
		c := value[i]
		switch {
		case c >= 'A' && c <= 'Z', c >= 'a' && c <= 'z', c >= '0' && c <= '9':
		case c == '.' || c == '_' || c == '-' || c == ':':
		default:
			return ErrInvalid
		}
	}
	return nil
}
