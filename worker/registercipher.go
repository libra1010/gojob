// Go SDK 的注册口令加密握手（与服务端 internal/registercrypto 同一份线格式实现）。
//
// 口径：**配了 AesKey 就一定加密**，不会因为服务端没启用而静默退回明文——静默降级等于
// 给链路上的中间人一个"抹掉加密"的开关（它只要挡掉 /worker/challenge 就行）。
// 所以升级顺序是先配服务端 GOJOB_WORKER_AES_KEYS、再配 worker；两侧都没配时才走明文。
package worker

import (
	"context"
	"errors"
	"fmt"

	"gojob/internal/protocol"
	"gojob/internal/registercrypto"
)

// sealRegister 握手两步：领一张一次性 challenge，把口令封进 AES-256-GCM 密文。
// 直接改写 req（口令字段清空），调用方只管把 req 发出去。
func (w *Worker) sealRegister(ctx context.Context, req *protocol.RegisterReq) error {
	raw, err := registercrypto.ParseKey(w.cfg.AesKey)
	if err != nil {
		return fmt.Errorf("worker.Config.AesKey 配置有误（需 64 位 hex 的 AES-256 key）: %w", err)
	}
	ch, err := post[protocol.ChallengeResp](w.client, ctx, "/worker/challenge", &protocol.ChallengeReq{
		AppId: req.AppId, InstanceId: req.InstanceId,
	})
	if err != nil {
		return fmt.Errorf("领取注册 challenge 失败，请确认服务端已配 GOJOB_WORKER_AES_KEYS: %w", err)
	}
	if ch.Data.Challenge == "" {
		return errors.New("服务端返回了空 challenge（多半是它没启用注册加密，而本节点配了 AesKey）")
	}
	kid, iv, ct, err := registercrypto.Seal(raw, registercrypto.Payload{
		Version:    registercrypto.PayloadVersion,
		AppId:      req.AppId,
		InstanceId: req.InstanceId,
		Challenge:  ch.Data.Challenge,
		Password:   w.cfg.Password,
	})
	if err != nil {
		return err
	}
	req.AuthScheme, req.Kid, req.Iv, req.Ciphertext, req.Challenge = registercrypto.SchemeAesGcm, kid, iv, ct, ch.Data.Challenge
	req.Password = "" // 口令只存在于密文里
	return nil
}
