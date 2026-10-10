package upstream

import "sync"

// singleflightCall 记录在途的单一调用状态
type singleflightCall struct {
	wg  sync.WaitGroup
	val any
	err error
}

// singleflightGroup 抑制对同一 key 的并发重复调用
type singleflightGroup struct {
	mu sync.Mutex
	m  map[string]*singleflightCall
}

// Do 执行 fn，如果已有相同的 key 正在执行，则后续并发调用会挂起等待第一个调用的结果并复用
func (g *singleflightGroup) Do(key string, fn func() (any, error)) (any, error) {
	g.mu.Lock()
	if g.m == nil {
		g.m = make(map[string]*singleflightCall)
	}
	if c, ok := g.m[key]; ok {
		g.mu.Unlock()
		c.wg.Wait()
		return c.val, c.err
	}
	c := new(singleflightCall)
	c.wg.Add(1)
	g.m[key] = c
	g.mu.Unlock()

	c.val, c.err = fn()
	c.wg.Done()

	g.mu.Lock()
	delete(g.m, key)
	g.mu.Unlock()

	return c.val, c.err
}
