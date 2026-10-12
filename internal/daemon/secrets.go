package daemon

import (
	"fmt"
	"os"
	"strings"
	"unicode/utf8"

	"github.com/kravlab/hostrunner/internal/protocol"
	"github.com/kravlab/hostrunner/internal/rules"
)

// minSecretLength is the shortest value a secret may have: a shorter one
// could not be masked without mangling the command's output, and no real
// token is that short, so it is most likely a broken source.
const minSecretLength = 8

// obtainSecrets obtains the value of every secret, fresh for this request,
// with trailing CR and LF trimmed. A secret that yields no usable value
// refuses the request with ExitHostrunError and a message naming the
// secret only; the reason, never the value, goes to the error for the log.
func obtainSecrets(secrets []rules.Secret) (map[string]string, *refusal) {
	if len(secrets) == 0 {
		return nil, nil
	}
	values := make(map[string]string, len(secrets))
	for _, sec := range secrets {
		value, err := obtain(sec)
		if err != nil {
			return nil, &refusal{protocol.ExitHostrunError, fmt.Sprintf("secret %q is not available", sec.Name), fmt.Errorf("secret %q: %w", sec.Name, err)}
		}
		value = strings.TrimRight(value, "\r\n")
		switch n := utf8.RuneCountInString(value); {
		case n == 0:
			return nil, &refusal{protocol.ExitHostrunError, fmt.Sprintf("secret %q is not available", sec.Name), fmt.Errorf("secret %q: the value is empty", sec.Name)}
		case n < minSecretLength:
			return nil, &refusal{protocol.ExitHostrunError, fmt.Sprintf("secret %q is too short to mask", sec.Name), fmt.Errorf("secret %q: the value has %d characters, fewer than %d", sec.Name, n, minSecretLength)}
		}
		values[sec.Name] = value
	}
	return values, nil
}

// obtain reads the raw value of sec from its source.
func obtain(sec rules.Secret) (string, error) {
	value, ok := os.LookupEnv(sec.Env)
	if !ok {
		return "", fmt.Errorf("environment variable %s is not set", sec.Env)
	}
	return value, nil
}

// secretNames returns the names of secrets, for the log.
func secretNames(secrets []rules.Secret) []string {
	names := make([]string, len(secrets))
	for i, sec := range secrets {
		names[i] = sec.Name
	}
	return names
}
