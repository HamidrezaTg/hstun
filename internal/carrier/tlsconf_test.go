package carrier

import (
	"regexp"
	"testing"
)

func TestDefaultUserAgentIsChromeShaped(t *testing.T) {
	re := regexp.MustCompile(`^Mozilla/5\.0 \([^)]+\) AppleWebKit/537\.36 \(KHTML, like Gecko\) Chrome/\d{3}\.0\.0\.0 Safari/537\.36$`)
	if !re.MatchString(DefaultUserAgent) {
		t.Fatalf("default User-Agent is not the frozen Chrome shape: %q", DefaultUserAgent)
	}
}
