package main

type PdhCounter struct {
	Path string
	Multiplier float64
}

// UnmarshalYAML will be called any time the PdhCounter struct is being
// unmarshaled. This function is being implemented so that PdhCounter can
// have a default value set for Multiplier
func (s *PdhCounter) UnmarshalYAML(unmarshal func(interface{}) error) error {
	type rawPdhCounter PdhCounter
	raw := rawPdhCounter{Multiplier:1.0} // Set default multiplier value of 1.0
	if err := unmarshal(&raw); err != nil {
		return err
	}

	*s = PdhCounter(raw)
	return nil
}
