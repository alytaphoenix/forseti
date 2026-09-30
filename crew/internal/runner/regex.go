package runner

import (
	"regexp"
	"sync"
)

var (
	regexMu    sync.Mutex
	regexCache = map[string]*regexp.Regexp{}
)

func compileCached(pattern string) (*regexp.Regexp, error) {
	regexMu.Lock()
	defer regexMu.Unlock()
	if re, ok := regexCache[pattern]; ok {
		return re, nil
	}
	re, err := regexp.Compile(pattern)
	if err != nil {
		return nil, err
	}
	regexCache[pattern] = re
	return re, nil
}
