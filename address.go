package smtpd

import (
	"fmt"
	"strings"
)

func parseAddress(src string) (string, error) {

	if src[0] != '<' || src[len(src)-1] != '>' {
		return "", fmt.Errorf("ill-formatted e-mail address: %s", src)
	}

	if strings.Count(src, "@") > 1 {
		return "", fmt.Errorf("ill-formatted e-mail address: %s", src)
	}

	return src[1 : len(src)-1], nil
}
