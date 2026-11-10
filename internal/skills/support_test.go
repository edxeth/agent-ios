package skills

// Test support of this package's tests only: nothing outside the tests
// uses it, so it lives here and not in the production files.

// Version is version of the embedded skills.
func Version() (string, error) {
	all, err := load()
	if err != nil {
		return "", err
	}
	return version(all), nil
}
