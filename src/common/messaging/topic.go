package messaging

import (
	"fmt"
	"strings"
)

func BuildTopic(m Message) string {
	return fmt.Sprintf("%s.%s.%s", m.App, m.Type, strings.ToLower(m.Operation))
}
