package openai

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"one-api/common"
	"one-api/common/config"
	"one-api/common/logger"
	"one-api/common/requester"
	"one-api/types"
	"strconv"
	"strings"
)

// 思维链转内容 http header key
const ThinkContentHeader = "X-Thinking-To-Content"

type OpenAIStreamHandler struct {
	Usage                     *types.Usage
	ModelName                 string
	isAzure                   bool
	EscapeJSON                bool
	Think2Content             bool
	IsFirstResponse           bool
	SendLastReasoningResponse bool
}

func (p *OpenAIProvider) CreateChatCompletion(request *types.ChatCompletionRequest) (openaiResponse *types.ChatCompletionResponse, errWithCode *types.OpenAIErrorWithStatusCode) {
	otherProcessing(request, p.GetOtherArg())

	req, errWithCode := p.GetRequestTextBody(config.RelayModeChatCompletions, request.Model, request)
	if errWithCode != nil {
		return nil, errWithCode
	}
	defer req.Body.Close()

	response := &OpenAIProviderChatResponse{}
	// 发送请求
	_, errWithCode = p.Requester.SendRequest(req, response, false)
	if errWithCode != nil {
		return nil, errWithCode
	}

	// 检测是否错误
	openaiErr := ErrorHandle(&response.OpenAIErrorResponse)
	if openaiErr != nil {
		errWithCode = &types.OpenAIErrorWithStatusCode{
			OpenAIError: *openaiErr,
			StatusCode:  http.StatusBadRequest,
		}
		return nil, errWithCode
	}

	if response.Usage == nil || response.Usage.CompletionTokens == 0 {
		response.Usage = &types.Usage{
			PromptTokens:     p.Usage.PromptTokens,
			CompletionTokens: 0,
			TotalTokens:      0,
		}
		// 那么需要计算
		response.Usage.CompletionTokens = common.CountTokenText(response.GetContent(), request.Model)
		response.Usage.TotalTokens = response.Usage.PromptTokens + response.Usage.CompletionTokens
	}

	*p.Usage = *response.Usage

	return &response.ChatCompletionResponse, nil
}

func (p *OpenAIProvider) CreateChatCompletionStream(request *types.ChatCompletionRequest) (requester.StreamReaderInterface[string], *types.OpenAIErrorWithStatusCode) {
	otherProcessing(request, p.GetOtherArg())
	streamOptions := request.StreamOptions
	// 如果支持流式返回Usage 则需要更改配置：
	if p.SupportStreamOptions {
		request.StreamOptions = &types.StreamOptions{
			IncludeUsage: true,
		}
	} else {
		// 避免误传导致报错
		request.StreamOptions = nil
	}
	req, errWithCode := p.GetRequestTextBody(config.RelayModeChatCompletions, request.Model, request)
	if errWithCode != nil {
		return nil, errWithCode
	}
	defer req.Body.Close()

	// 恢复原来的配置
	request.StreamOptions = streamOptions

	// 发送请求
	resp, errWithCode := p.Requester.SendRequestRaw(req)
	if errWithCode != nil {
		return nil, errWithCode
	}

	think2content := p.getThink2content()
	logger.LogInfo(p.Context, fmt.Sprintf("Header %s: %v", ThinkContentHeader, think2content))

	chatHandler := OpenAIStreamHandler{
		Usage:                     p.Usage,
		ModelName:                 request.Model,
		isAzure:                   p.IsAzure,
		EscapeJSON:                p.StreamEscapeJSON,
		Think2Content:             think2content,
		IsFirstResponse:           true,
		SendLastReasoningResponse: false,
	}

	return requester.RequestStream[string](p.Requester, resp, chatHandler.HandlerChatStream)
}

func (p *OpenAIProvider) getThink2content() bool {
	think2content := false
	// 优先使用传入的http header
	value := ""
	if p.Context != nil {
		value = p.Context.Request.Header.Get(ThinkContentHeader)
	}
	if value == "" {
		// 获取渠道配置的请求头
		value, _ = p.GetRequestHeaders()[ThinkContentHeader]
	}
	var err error
	think2content, err = strconv.ParseBool(value)
	if err != nil {
		logger.LogError(p.Context, fmt.Sprintf("Header %s: %v", ThinkContentHeader, value))
	}
	return think2content
}

func (h *OpenAIStreamHandler) HandlerChatStream(rawLine *[]byte, dataChan chan string, errChan chan error) {
	// 如果rawLine 前缀不为data:，则直接返回
	if !strings.HasPrefix(string(*rawLine), "data:") {
		*rawLine = nil
		return
	}

	// 去除前缀
	*rawLine = (*rawLine)[5:]
	*rawLine = bytes.TrimSpace(*rawLine)

	// 如果等于 DONE 则结束
	if string(*rawLine) == "[DONE]" {
		errChan <- io.EOF
		*rawLine = requester.StreamClosed
		return
	}

	var openaiResponse OpenAIProviderChatStreamResponse
	err := json.Unmarshal(*rawLine, &openaiResponse)
	if err != nil {
		errChan <- common.ErrorToOpenAIError(err)
		return
	}

	aiError := ErrorHandle(&openaiResponse.OpenAIErrorResponse)
	if aiError != nil {
		errChan <- aiError
		return
	}

	if openaiResponse.Usage != nil {
		if openaiResponse.Usage.CompletionTokens > 0 {
			*h.Usage = *openaiResponse.Usage
		}

		if len(openaiResponse.Choices) == 0 {
			*rawLine = nil
			return
		}
	} else {
		if len(openaiResponse.Choices) > 0 && openaiResponse.Choices[0].Usage != nil {
			if openaiResponse.Choices[0].Usage.CompletionTokens > 0 {
				*h.Usage = *openaiResponse.Choices[0].Usage
			}
		} else {
			if h.Usage.TotalTokens == 0 {
				h.Usage.TotalTokens = h.Usage.PromptTokens
			}
			countTokenText := common.CountTokenText(openaiResponse.GetResponseText(), h.ModelName)
			h.Usage.CompletionTokens += countTokenText
			h.Usage.TotalTokens += countTokenText
		}
	}

	if h.Think2Content {
		// Handle first message
		if h.IsFirstResponse {
			if data, err := JsonMarshalNoSetEscapeHTML(openaiResponse.ChatCompletionStreamResponse); err == nil {
				dataChan <- string(data)
			}

			response := openaiResponse.ChatCompletionStreamResponse.Copy()
			for j := range response.Choices {
				delta := types.ChatCompletionStreamChoiceDelta{
					Content:          "<think>\n",
					ReasoningContent: "",
				}
				response.Choices[j].Delta = delta
			}
			if data, err := JsonMarshalNoSetEscapeHTML(response); err == nil {
				dataChan <- string(data)
			}
			h.IsFirstResponse = false
			return
		}

		// Process each choice
		for i, choice := range openaiResponse.ChatCompletionStreamResponse.Choices {

			// Handle transition from thinking to content
			if len(choice.Delta.Content) > 0 && !h.SendLastReasoningResponse {
				h.SendLastReasoningResponse = true
				response1 := openaiResponse.ChatCompletionStreamResponse.Copy()
				response2 := openaiResponse.ChatCompletionStreamResponse.Copy()
				sendThinkData(response2, dataChan, "\n</think>")

				if err := sendThinkData(response1, dataChan, "\n\n"); err == nil {
					return
				}
			}

			// Convert reasoning content to regular content
			if len(choice.Delta.ReasoningContent) > 0 {
				openaiResponse.ChatCompletionStreamResponse.Choices[i].Delta.Content = choice.Delta.ReasoningContent
				openaiResponse.ChatCompletionStreamResponse.Choices[i].Delta.ReasoningContent = ""
			}
		}
	}

	if h.EscapeJSON || h.Think2Content {
		if data, err := JsonMarshalNoSetEscapeHTML(openaiResponse.ChatCompletionStreamResponse); err == nil {
			dataChan <- string(data)
			return
		}
	}
	dataChan <- string(*rawLine)
}

func sendThinkData(response *types.ChatCompletionStreamResponse, dataChan chan string, content string) error {
	for j := range response.Choices {
		response.Choices[j].Delta.Content = content
		response.Choices[j].Delta.ReasoningContent = ""
	}
	data, err := JsonMarshalNoSetEscapeHTML(response)
	if err == nil {
		dataChan <- string(data)
	}
	return err
}

func otherProcessing(request *types.ChatCompletionRequest, otherArg string) {
	if (strings.HasPrefix(request.Model, "o1") || strings.HasPrefix(request.Model, "o3")) && request.MaxTokens > 0 {
		request.MaxCompletionTokens = request.MaxTokens
		request.MaxTokens = 0

		if strings.HasPrefix(request.Model, "o3") {
			request.Temperature = nil
			if otherArg != "" {
				request.ReasoningEffort = &otherArg
			}
		}
	}
}

func JsonMarshalNoSetEscapeHTML(data interface{}) ([]byte, error) {
	bf := bytes.NewBuffer([]byte{})
	jsonEncoder := json.NewEncoder(bf)
	jsonEncoder.SetEscapeHTML(false)
	if err := jsonEncoder.Encode(data); err != nil {
		return nil, err
	}

	return bf.Bytes(), nil
}
