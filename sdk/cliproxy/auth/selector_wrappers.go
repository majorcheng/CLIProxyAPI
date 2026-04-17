package auth

import (
	"context"
	"time"

	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/executor"
)

// StoppableSelector 表示 selector 持有后台资源，需要在替换或停机时显式释放。
type StoppableSelector interface {
	Selector
	Stop()
}

// selectorWrapper 允许外层包装器暴露下一层 selector，方便查找底层能力。
type selectorWrapper interface {
	UnwrapSelector() Selector
}

// PriorityZeroOverrideSelector 在最高 ready bucket 为 0 时切换到专用策略。
type PriorityZeroOverrideSelector struct {
	base         Selector
	priorityZero Selector
}

// NewPriorityZeroOverrideSelector 创建 priority=0 专用覆盖层。
func NewPriorityZeroOverrideSelector(base, priorityZero Selector) Selector {
	if base == nil {
		base = &RoundRobinSelector{}
	}
	if priorityZero == nil {
		return base
	}
	return &PriorityZeroOverrideSelector{base: base, priorityZero: priorityZero}
}

// UnwrapSelector 返回被包装的基础 selector。
func (s *PriorityZeroOverrideSelector) UnwrapSelector() Selector {
	if s == nil {
		return nil
	}
	return s.base
}

// Pick 在 priority=0 bucket 上应用覆盖策略，其余情况沿用基础策略。
func (s *PriorityZeroOverrideSelector) Pick(ctx context.Context, provider, model string, opts cliproxyexecutor.Options, auths []*Auth) (*Auth, error) {
	if s == nil || s.base == nil {
		return nil, &Error{Code: "auth_not_found", Message: "selector not configured"}
	}
	if !highestReadyPriorityIsZero(auths, model) || s.priorityZero == nil {
		return s.base.Pick(ctx, provider, model, opts, auths)
	}
	return s.priorityZero.Pick(ctx, provider, model, opts, auths)
}

// ObserveResult 透传反馈型路由统计，保持 success-rate 等策略可用。
func (s *PriorityZeroOverrideSelector) ObserveResult(result Result, now time.Time) {
	if observer, ok := selectorResultObserver(s.base); ok {
		observer.ObserveResult(result, now)
	}
}

// Stop 递归停止内部 selector，释放后台资源。
func (s *PriorityZeroOverrideSelector) Stop() {
	if s == nil {
		return
	}
	stopSelector(s.priorityZero)
	stopSelector(s.base)
}

func highestReadyPriorityIsZero(auths []*Auth, model string) bool {
	availableByPriority, _, _ := collectAvailableByPriority(auths, model, time.Now())
	if len(availableByPriority) == 0 {
		return false
	}
	bestPriority := 0
	found := false
	for priority := range availableByPriority {
		if !found || priority > bestPriority {
			bestPriority = priority
			found = true
		}
	}
	return found && bestPriority == 0
}

func selectorResultObserver(selector Selector) (ResultObserver, bool) {
	observer, ok := selector.(ResultObserver)
	return observer, ok && observer != nil
}

func stopSelector(selector Selector) {
	if selector == nil {
		return
	}
	if stopper, ok := selector.(StoppableSelector); ok && stopper != nil {
		stopper.Stop()
		return
	}
	if wrapper, ok := selector.(selectorWrapper); ok {
		stopSelector(wrapper.UnwrapSelector())
	}
}

func sessionAffinitySelectorOf(selector Selector) *SessionAffinitySelector {
	for selector != nil {
		affinity, ok := selector.(*SessionAffinitySelector)
		if ok && affinity != nil {
			return affinity
		}
		wrapper, ok := selector.(selectorWrapper)
		if !ok {
			return nil
		}
		selector = wrapper.UnwrapSelector()
	}
	return nil
}

func simHashSelectorOf(selector Selector) *SimHashSelector {
	for selector != nil {
		simhash, ok := selector.(*SimHashSelector)
		if ok && simhash != nil {
			return simhash
		}
		wrapper, ok := selector.(selectorWrapper)
		if !ok {
			return nil
		}
		selector = wrapper.UnwrapSelector()
	}
	return nil
}

func builtInSelectorStrategy(selector Selector) (schedulerStrategy, bool) {
	if selector == nil {
		return schedulerStrategyRoundRobin, true
	}
	for selector != nil {
		switch selector.(type) {
		case *RoundRobinSelector:
			return schedulerStrategyRoundRobin, true
		case *FillFirstSelector:
			return schedulerStrategyFillFirst, true
		}
		wrapper, ok := selector.(selectorWrapper)
		if !ok {
			return schedulerStrategyCustom, false
		}
		selector = wrapper.UnwrapSelector()
	}
	return schedulerStrategyRoundRobin, true
}
