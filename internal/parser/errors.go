package parser

import "errors"

// errUnrecognized 无法从消息中解析出有效剧名
var errUnrecognized = errors.New("无法解析剧名，需人工认领")

// IsUnrecognized 判断是否为无法识别错误
func IsUnrecognized(err error) bool { return errors.Is(err, errUnrecognized) }
