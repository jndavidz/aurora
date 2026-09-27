package provider

import "errors"

var errFakeUpstream = errors.New("fake upstream failure")

// errAlwaysSender 测试用恒败 sender:每次 Send 都返回同一 error
// (降级「重试一次仍失败」用)。请求照常记入 inner.sent 供断言。
type errAlwaysSender struct {
	inner *fakeChatSender
}

func (f *errAlwaysSender) Send(token string, req deepseekSenderReq) (*preConsumedStream, error) {
	f.inner.sent = append(f.inner.sent, &req)
	return nil, errFakeUpstream
}

// failOnceSender 装饰 fakeChatSender:failFirst=true 时首次 Send 报错。
// 请求仅由装饰层记入 inner.sent(装饰层必被调,inner.Send 不一定被调,
// 双重记账会导致 sent 数翻倍)。
type failOnceSender struct {
	inner     *fakeChatSender
	failFirst *bool
	count     int
}

func (f *failOnceSender) Send(token string, req deepseekSenderReq) (*preConsumedStream, error) {
	f.count++
	if *f.failFirst && f.count == 1 {
		f.inner.sent = append(f.inner.sent, &req)
		return nil, errFakeUpstream
	}
	return f.inner.Send(token, req)
}
