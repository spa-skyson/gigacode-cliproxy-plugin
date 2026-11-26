package gigacode

import (
	"context"
	"encoding/json"
	"strings"
	"time"
)

// handleModelStatic — model.static: у провайдера нет статических моделей,
// каталог публикуется только по учётным записям.
func handleModelStatic(core *Core, raw []byte) ([]byte, error) {
	return okEnvelope(ModelResponse{
		Provider: providerID,
		Models:   []ModelInfo{},
	})
}

// handleModelForAuth — model.for_auth: модели из шага 3 флоу.
// При свежем сеансе в менеджере токенов модели отдаются из кэша без
// обращений в сеть; при любой неудаче флоу — пустой список без ошибки.
func handleModelForAuth(core *Core, raw []byte) ([]byte, error) {
	var req AuthModelRequest
	if err := json.Unmarshal(raw, &req); err != nil {
		core.log("warn", "Некорректный запрос model.for_auth: "+sanitizeText(err.Error(), 512), nil)
		return nil, newErr(codeInvalidRequest, "Некорректный запрос", 400)
	}
	if req.AuthProvider != providerID {
		// Чужой провайдер — пустой список.
		return okEnvelope(ModelResponse{Provider: providerID, Models: []ModelInfo{}})
	}
	deviceCode, err := parseStorageJSON(req.StorageJSON)
	if err != nil {
		return nil, err
	}
	cfg := core.config()
	margin := time.Duration(cfg.RefreshMarginSeconds) * time.Second

	snap, cached := core.store.peekFresh(deviceCode, margin)
	if !cached {
		tr := core.transportFor(req.HostCallbackID)
		snap, err = core.ensureAuth(context.Background(), tr, cfg, deviceCode, false)
		if err != nil {
			// Успешный конверт с пустым списком: хост продолжит работать,
			// повтор будет по расписанию auth.refresh.
			core.log("warn", "Модели GigaCode недоступны: "+sanitizeText(err.Error(), 512), nil)
			return okEnvelope(ModelResponse{Provider: providerID, Models: []ModelInfo{}})
		}
	}

	models := make([]ModelInfo, 0, len(snap.Models))
	for _, rawModel := range snap.Models {
		info := modelInfoFromRaw(rawModel)
		if info == nil {
			continue // не объект или без непустого поля model
		}
		models = append(models, *info)
	}
	return okEnvelope(ModelResponse{Provider: providerID, Models: models})
}

// modelInfoFromRaw — преобразование элемента chat.models[].
func modelInfoFromRaw(rawModel map[string]any) *ModelInfo {
	id, ok := rawModel["model"].(string)
	if !ok || strings.TrimSpace(id) == "" {
		return nil
	}
	info := &ModelInfo{
		ID:                         id,
		Object:                     "model",
		OwnedBy:                    providerID,
		DisplayName:                id,
		SupportedGenerationMethods: []string{"chat"},
		UserDefined:                true,
	}
	if name, ok := rawModel["name"].(string); ok && strings.TrimSpace(name) != "" {
		info.DisplayName = name
	}
	if description, ok := rawModel["description"].(string); ok {
		info.Description = description
	}
	return info
}
