package usage

// EstimateTokens 按字符类型折算 token：实测中文 0.62 token/字、英文约 4 字符/token
// （2026-10 长上下文针测试：400k 汉字 = 249,847 token）。纯 chars/4 对中文低估 2.5 倍。
//
// 用途：上游 Tabbit 的 run_completed 帧**不报任何 token 数**（实抓确认，见 README 的
// usage 一条），网关不编造——只在上游一个数都没给时，用这条估算给面板/客户端一个
// 量级参考，并在 usage 里带 estimated:true。调用方必须尊重这个标记。
// shortcut: 纯字符估算，比真实分词偏 10% 上下；接真分词器时替换本函数即可。
func EstimateTokens(runes []rune) int {
	n := 0.0
	for _, r := range runes {
		if r > 0x2E80 { // CJK / 假名 / 谚文
			n += 0.62
		} else {
			n += 0.25
		}
	}
	return int(n + 0.999)
}
