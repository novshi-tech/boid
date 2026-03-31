package sandbox

func WireProxy(allowedDomains []string) *Proxy {
	return NewProxy(allowedDomains)
}
