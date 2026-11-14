package parser

import "errors"

var errUnrecognized = errors.New("无法解析剧名，需人工认领")

func IsUnrecognized(err error) bool { return errors.Is(err, errUnrecognized) }
