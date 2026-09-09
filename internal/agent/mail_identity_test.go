package agent

import (
	"strings"
	"testing"
	"testing/quick"

	"agentgo/internal/mailbox"
)

func TestInformationIdentityReachesModelContext(t *testing.T) {
	property := func(n uint64) bool {
		id := stableLoopID("message", boundedLoopIdentity(string(rune(n%26+'a'))))
		text := formatMailMessages([]mailbox.Message{{ID: id, ReplyTo: "original",
			Type: mailbox.MsgTypeReply, DeliveryOnly: true, Content: "执行事实"}})
		return strings.Contains(text, `message_id="`+id+`"`) &&
			strings.Contains(text, `reply_to="original"`) && strings.Contains(text, `delivery_only="true"`)
	}
	if err := quick.Check(property, nil); err != nil {
		t.Fatalf("消息身份未传入模型上下文：%v", err)
	}
}
