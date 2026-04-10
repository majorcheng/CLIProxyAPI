package executor

import (
	"fmt"
	"regexp"
	"strings"

	xxHash64 "github.com/pierrec/xxHash/xxHash64"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

const claudeCCHSeed uint64 = 0x6E52736AC806831E

var claudeBillingHeaderCCHPattern = regexp.MustCompile(`\bcch=([0-9a-f]{5});`)

// signAnthropicMessagesBody 会把 system[0] 里的 cch 占位符替换成请求体签名，
// 让 OAuth 形态更贴近真实 Claude Code 的 billing header。
func signAnthropicMessagesBody(body []byte) []byte {
	billingHeader := gjson.GetBytes(body, "system.0.text").String()
	if !strings.HasPrefix(billingHeader, "x-anthropic-billing-header:") {
		return body
	}
	if !claudeBillingHeaderCCHPattern.MatchString(billingHeader) {
		return body
	}

	unsignedBillingHeader := claudeBillingHeaderCCHPattern.ReplaceAllString(billingHeader, "cch=00000;")
	unsignedBody, err := sjson.SetBytes(body, "system.0.text", unsignedBillingHeader)
	if err != nil {
		return body
	}

	cch := fmt.Sprintf("%05x", xxHash64.Checksum(unsignedBody, claudeCCHSeed)&0xFFFFF)
	signedBillingHeader := claudeBillingHeaderCCHPattern.ReplaceAllString(unsignedBillingHeader, "cch="+cch+";")
	signedBody, err := sjson.SetBytes(unsignedBody, "system.0.text", signedBillingHeader)
	if err != nil {
		return unsignedBody
	}
	return signedBody
}
