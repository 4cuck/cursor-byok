package interaction

import (
	"bytes"
	"encoding/json"
	"fmt"

	"cursor/gen/agentv1"

	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
)

// UnmarshalProtoJSONArgs 解析交互工具参数。
// Cursor 账户把 InteractionQuery 用 protojson 编码，字段名是驼峰（targetModeId、searchTerm）。
// 本地工具 schema 走生成结构的 encoding/json 标签，字段名是蛇形（target_mode_id、search_term）。
// protojson 两种名字都接受；遇到它拒绝的载荷再退回 encoding/json。
func UnmarshalProtoJSONArgs(raw []byte, msg proto.Message) error {
	if msg == nil {
		return fmt.Errorf("proto message is required")
	}
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 {
		return nil
	}
	if err := (protojson.UnmarshalOptions{DiscardUnknown: true}).Unmarshal(trimmed, msg); err == nil {
		return nil
	}
	proto.Reset(msg)
	return json.Unmarshal(trimmed, msg)
}

// unmarshalPrManagementArgs 优先用 protojson 还原 Cursor 账户的 oneof（createPr / updatePr）。
// 本地路径用 encoding/json 编码，oneof 没有 json 标签，protojson 会把它当成未知字段丢掉；
// 这时退回原来的解码，避免已有结果丢字段。
func unmarshalPrManagementArgs(raw []byte) *agentv1.PrManagementArgs {
	args := &agentv1.PrManagementArgs{}
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 {
		return args
	}
	if err := (protojson.UnmarshalOptions{DiscardUnknown: true}).Unmarshal(trimmed, args); err == nil && args.GetAction() != nil {
		return args
	}
	args.Reset()
	_ = json.Unmarshal(trimmed, args)
	return args
}
