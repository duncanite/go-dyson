package mqtt

import (
	"fmt"
	"strings"
	"unicode/utf8"
)

// longest is the most bytes the protocol carries a topic or a filter in.
const longest = 65535

// The characters a filter gives a meaning to.
const (
	separator = "/"
	oneLevel  = "+"
	allLevels = "#"
)

// CheckTopic says whether a message can be published to topic: text of at
// least one character and 65535 bytes at most, with no NUL and neither of
// the wildcards. What it refuses is an ErrArgument.
func CheckTopic(topic string) error {
	if err := checkText(topic, "topic"); err != nil {
		return err
	}

	if strings.ContainsAny(topic, oneLevel+allLevels) {
		return fmt.Errorf("%w: the topic %q holds a wildcard", ErrArgument, topic)
	}

	return nil
}

// CheckFilter says whether filter can be subscribed to: a topic's text,
// where a level that is + stands for any one level, and a last level that
// is # for that level and all below it. A wildcard is a whole level or it
// is refused, with an ErrArgument.
func CheckFilter(filter string) error {
	if err := checkText(filter, "filter"); err != nil {
		return err
	}

	levels := strings.Split(filter, separator)

	for index, level := range levels {
		switch {
		case level == oneLevel, level == allLevels && index == len(levels)-1:
		case strings.Contains(level, allLevels):
			return fmt.Errorf("%w: in the filter %q, # is not the whole of the last level", ErrArgument, filter)
		case strings.Contains(level, oneLevel):
			return fmt.Errorf("%w: in the filter %q, + is not the whole of a level", ErrArgument, filter)
		}
	}

	return nil
}

func checkText(text, what string) error {
	switch {
	case text == "":
		return fmt.Errorf("%w: an empty %s", ErrArgument, what)
	case len(text) > longest:
		return fmt.Errorf("%w: a %s of %d bytes, over %d", ErrArgument, what, len(text), longest)
	case !utf8.ValidString(text) || strings.ContainsRune(text, 0):
		return fmt.Errorf("%w: a %s that is not text: %q", ErrArgument, what, text)
	}

	return nil
}

// Match says whether a filter matches a topic, as a broker decides it. A
// topic that begins with $ is the broker's own, and no filter that begins
// with a wildcard matches it. A filter or a topic that Check refuses
// matches nothing.
func Match(filter, topic string) bool {
	if CheckFilter(filter) != nil || CheckTopic(topic) != nil {
		return false
	}

	wanted, given := strings.Split(filter, separator), strings.Split(topic, separator)

	if strings.HasPrefix(topic, "$") && (wanted[0] == oneLevel || wanted[0] == allLevels) {
		return false
	}

	for index, level := range wanted {
		switch {
		case level == allLevels:
			// A/# matches A too: the level above, and all below.
			return true
		case index >= len(given):
			return false
		case level != oneLevel && level != given[index]:
			return false
		}
	}

	return len(wanted) == len(given)
}
