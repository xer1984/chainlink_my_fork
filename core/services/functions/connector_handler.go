package functions

import (
	"context"
	"crypto/ecdsa"
	"encoding/json"
	"fmt"

	"github.com/smartcontractkit/chainlink/v2/core/logger"
	"github.com/smartcontractkit/chainlink/v2/core/services/gateway/api"
	"github.com/smartcontractkit/chainlink/v2/core/services/gateway/common"
	"github.com/smartcontractkit/chainlink/v2/core/services/gateway/connector"
	"github.com/smartcontractkit/chainlink/v2/core/services/gateway/handlers/functions"
	"github.com/smartcontractkit/chainlink/v2/core/services/s4"
	"github.com/smartcontractkit/chainlink/v2/core/utils"

	ethCommon "github.com/ethereum/go-ethereum/common"
)

type functionsConnectorHandler struct {
	utils.StartStopOnce

	connector   connector.GatewayConnector
	signerKey   *ecdsa.PrivateKey
	nodeAddress string
	storage     s4.Storage
	allowlist   functions.OnchainAllowlist
	lggr        logger.Logger
}

const (
	methodSecretsSet  = "secrets_set"
	methodSecretsList = "secrets_list"
)

var (
	_ connector.Signer                  = &functionsConnectorHandler{}
	_ connector.GatewayConnectorHandler = &functionsConnectorHandler{}
)

func NewFunctionsConnectorHandler(nodeAddress string, signerKey *ecdsa.PrivateKey, storage s4.Storage, allowlist functions.OnchainAllowlist, lggr logger.Logger) *functionsConnectorHandler {
	return &functionsConnectorHandler{
		nodeAddress: nodeAddress,
		signerKey:   signerKey,
		storage:     storage,
		allowlist:   allowlist,
		lggr:        lggr.Named("functionsConnectorHandler"),
	}
}

func (h *functionsConnectorHandler) SetConnector(connector connector.GatewayConnector) {
	h.connector = connector
}

func (h *functionsConnectorHandler) Sign(data ...[]byte) ([]byte, error) {
	return common.SignData(h.signerKey, data...)
}

func (h *functionsConnectorHandler) HandleGatewayMessage(ctx context.Context, gatewayId string, msg *api.Message) {
	body := &msg.Body
	fromAddr := ethCommon.HexToAddress(body.Sender)
	if !h.allowlist.Allow(fromAddr) {
		h.lggr.Errorw("allowlist prevented the request from this address", "id", gatewayId, "address", fromAddr)
		return
	}

	h.lggr.Debugw("handling gateway request", "id", gatewayId, "method", body.Method)

	switch body.Method {
	case methodSecretsList:
		h.handleSecretsList(ctx, gatewayId, body, fromAddr)
	case methodSecretsSet:
		h.handleSecretsSet(ctx, gatewayId, body, fromAddr)
	default:
		h.lggr.Errorw("unsupported method", "id", gatewayId, "method", body.Method)
	}
}

func (h *functionsConnectorHandler) Start(ctx context.Context) error {
	return h.StartOnce("FunctionsConnectorHandler", func() error {
		return h.allowlist.Start(ctx)
	})
}

func (h *functionsConnectorHandler) Close() error {
	return h.StopOnce("FunctionsConnectorHandler", func() error {
		return h.allowlist.Close()
	})
}

func (h *functionsConnectorHandler) handleSecretsList(ctx context.Context, gatewayId string, body *api.MessageBody, fromAddr ethCommon.Address) {
	type ListRow struct {
		SlotID     uint   `json:"slot_id"`
		Version    uint64 `json:"version"`
		Expiration int64  `json:"expiration"`
	}

	type ListResponse struct {
		Success      bool      `json:"success"`
		ErrorMessage string    `json:"error_message,omitempty"`
		Rows         []ListRow `json:"rows,omitempty"`
	}

	var response ListResponse
	snapshot, err := h.storage.List(ctx, fromAddr)
	if err == nil {
		response.Success = true
		response.Rows = make([]ListRow, len(snapshot))
		for i, row := range snapshot {
			response.Rows[i] = ListRow{
				SlotID:     row.SlotId,
				Version:    row.Version,
				Expiration: row.Expiration,
			}
		}
	} else {
		response.ErrorMessage = fmt.Sprintf("Failed to list secrets: %v", err)
	}

	if err := h.sendResponse(ctx, gatewayId, body, response); err != nil {
		h.lggr.Errorw("failed to send response to gateway", "id", gatewayId, "error", err)
	}
}

func (h *functionsConnectorHandler) handleSecretsSet(ctx context.Context, gatewayId string, body *api.MessageBody, fromAddr ethCommon.Address) {
	type SetRequest struct {
		SlotID     uint   `json:"slot_id"`
		Version    uint64 `json:"version"`
		Expiration int64  `json:"expiration"`
		Payload    []byte `json:"payload"`
		Signature  []byte `json:"signature"`
	}

	type SetResponse struct {
		Success      bool   `json:"success"`
		ErrorMessage string `json:"error_message,omitempty"`
	}

	var request SetRequest
	var response SetResponse
	err := json.Unmarshal(body.Payload, &request)
	if err == nil {
		key := s4.Key{
			Address: fromAddr,
			SlotId:  request.SlotID,
			Version: request.Version,
		}
		record := s4.Record{
			Expiration: request.Expiration,
			Payload:    request.Payload,
		}
		err = h.storage.Put(ctx, &key, &record, request.Signature)
		if err == nil {
			response.Success = true
		} else {
			response.ErrorMessage = fmt.Sprintf("Failed to set secret: %v", err)
		}
	} else {
		response.ErrorMessage = fmt.Sprintf("Bad request to set secret: %v", err)
	}

	if err := h.sendResponse(ctx, gatewayId, body, response); err != nil {
		h.lggr.Errorw("failed to send response to gateway", "id", gatewayId, "error", err)
	}
}

func (h *functionsConnectorHandler) sendResponse(ctx context.Context, gatewayId string, requestBody *api.MessageBody, payload any) error {
	payloadJson, err := json.Marshal(payload)
	if err != nil {
		return err
	}

	msg := &api.Message{
		Body: api.MessageBody{
			MessageId: requestBody.MessageId,
			DonId:     requestBody.DonId,
			Method:    requestBody.Method,
			Sender:    h.nodeAddress,
			Payload:   payloadJson,
		},
	}
	if err = msg.Sign(h.signerKey); err != nil {
		return err
	}

	err = h.connector.SendToGateway(ctx, gatewayId, msg)
	if err == nil {
		h.lggr.Debugw("sent to gateway", "id", gatewayId, "messageId", requestBody.MessageId, "donId", requestBody.DonId, "method", requestBody.Method)
	}
	return err
}
