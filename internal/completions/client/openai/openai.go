package openai

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"

	"github.com/pkoukk/tiktoken-go"

	"github.com/sourcegraph/sourcegraph/internal/completions/tokenizer"
	"github.com/sourcegraph/sourcegraph/internal/completions/types"
	"github.com/sourcegraph/sourcegraph/internal/httpcli"
	"github.com/sourcegraph/sourcegraph/internal/rcache"
	"github.com/sourcegraph/sourcegraph/internal/redispool"
	"github.com/sourcegraph/sourcegraph/lib/errors"
)

func NewClient(cli httpcli.Doer, endpoint, accessToken string) types.CompletionsClient {
	return &openAIChatCompletionStreamClient{
		cli:         cli,
		accessToken: accessToken,
		endpoint:    endpoint,
	}
}

type openAIChatCompletionStreamClient struct {
	cli         httpcli.Doer
	accessToken string
	endpoint    string
}

type TokenCounter interface {
	TryAdder(ctx context.Context) error
}

func NewTokenCounter(rstore redispool.KeyValue) TokenCounter {
	return &tokenCounter{rstore: rstore}
}

type tokenCounter struct {
	rstore redispool.KeyValue
}

func (r *tokenCounter) TryAdder(ctx context.Context) (err error) {
	rstore := r.rstore.WithContext(ctx)
	key := "hellomox"
	if _, err := rstore.Incr(key); err != nil {
		return errors.Wrap(err, "failed to increment rate limit counter")
	}

	currentUsage, _ := rstore.Get(key).Int()
	fmt.Println("this is the first usage", currentUsage)

	if _, err := rstore.Incr(key); err != nil {
		return errors.Wrap(err, "failed to increment rate limit counter")
	}

	currentUsage, _ = rstore.Get(key).Int()
	fmt.Println("this is the second  usage bro ", currentUsage)

	return nil
}

func (c *openAIChatCompletionStreamClient) Complete(
	ctx context.Context,
	feature types.CompletionsFeature,
	requestParams types.CompletionRequestParameters,
) (*types.CompletionResponse, error) {
	var resp *http.Response
	var err error
	defer (func() {
		if resp != nil {
			resp.Body.Close()
		}
	})()

	if feature == types.CompletionsFeatureCode {
		resp, err = c.makeCompletionRequest(ctx, requestParams, false)
	} else {
		resp, err = c.makeRequest(ctx, requestParams, false)
	}
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	var response openaiResponse
	if err := json.NewDecoder(resp.Body).Decode(&response); err != nil {
		return nil, err
	}

	if len(response.Choices) == 0 {
		// Empty response.
		return &types.CompletionResponse{}, nil
	}

	return &types.CompletionResponse{
		Completion: response.Choices[0].Text,
		StopReason: response.Choices[0].FinishReason,
	}, nil
}

func (c *openAIChatCompletionStreamClient) Stream(
	ctx context.Context,
	feature types.CompletionsFeature,
	requestParams types.CompletionRequestParameters,
	sendEvent types.SendCompletionEvent,
) error {
	var resp *http.Response
	var err error

	tokenCounter := NewTokenCounter(redispool.Cache)
	_ = tokenCounter.TryAdder(ctx)
	fmt.Println(tokenCounter)

	tokenizer, err := tokenizer.NewAnthropicClaudeTokenizer()
	if err != nil {
		return nil
	}
	fmt.Println("tokenizer for me ", tokenizer)
	defer (func() {
		if resp != nil {
			resp.Body.Close()
		}
	})()

	tokenCounterCache := rcache.NewWithTTL("LLMUsage", 1800)

	if feature == types.CompletionsFeatureCode {
		resp, err = c.makeCompletionRequest(ctx, requestParams, true)
	} else {
		resp, err = c.makeRequest(ctx, requestParams, true)
	}
	if err != nil {
		return err
	}
	dec := NewDecoder(resp.Body)
	var content string
	var ev types.CompletionResponse
	for dec.Scan() {
		if ctx.Err() != nil && ctx.Err() == context.Canceled {
			return nil
		}

		data := dec.Data()
		// Gracefully skip over any data that isn't JSON-like.
		if !bytes.HasPrefix(data, []byte("{")) {
			continue
		}

		var event openaiResponse
		if err := json.Unmarshal(data, &event); err != nil {
			return errors.Errorf("failed to decode event payload: %w - body: %s", err, string(data))
		}

		if len(event.Choices) > 0 {
			if feature == types.CompletionsFeatureCode {
				content += event.Choices[0].Text
			} else {
				content += event.Choices[0].Delta.Content
			}
			ev = types.CompletionResponse{
				Completion: content,
				StopReason: event.Choices[0].FinishReason,
			}
			err = sendEvent(ev)
			if err != nil {
				return err
			}
		}
	}

	tokencalculator(inputText(requestParams.Messages), ev.Completion, *tokenCounterCache, requestParams, feature)
	fmt.Println("successfuly request things", ev)
	return dec.Err()
}

func tokencalculator(inputText string, outputText string, tokenCounterCache rcache.Cache,
	requestParams types.CompletionRequestParameters, feature types.CompletionsFeature) {
	fmt.Println("Starting token calculation")
	encoding := "cl100k_base"
	fmt.Println("Encoding set to:", encoding)
	tke, err := tiktoken.GetEncoding(encoding)
	if err != nil {
		fmt.Println("Error getting encoding:", err)
		return
	}
	inputTokenLen := len(tke.Encode(inputText, nil, nil))
	fmt.Println("Input token length:", inputTokenLen)
	outputTokenLen := len(tke.Encode(outputText, nil, nil))
	fmt.Println("Output token length:", outputTokenLen)
	// set a variable value like
	var requestTypeDescription string

	if requestParams.Stream != nil && *requestParams.Stream {
		requestTypeDescription = "stream"
	} else {
		requestTypeDescription = "non-stream"
	}
	fmt.Println("Request type description:", requestTypeDescription)
	baseKey := requestParams.Model + string(feature) + requestTypeDescription
	fmt.Println("Base key:", baseKey)
	inputTokenKey := baseKey + "input"
	outputTokenKey := baseKey + "output"
	fmt.Println("Input token key:", inputTokenKey)
	fmt.Println("Output token key:", outputTokenKey)
	inputTokens, _ := tokenCounterCache.GetInt(inputTokenKey)
	outputTokens, _ := tokenCounterCache.GetInt(outputTokenKey)
	fmt.Println("Current input tokens:", inputTokens)
	fmt.Println("Current output tokens:", outputTokens)

	newInputTokens := inputTokens + inputTokenLen
	newOutputTokens := outputTokens + outputTokenLen
	fmt.Println("New input tokens:", newInputTokens)
	fmt.Println("New output tokens:", newOutputTokens)
	tokenCounterCache.SetInt(inputTokenKey, newInputTokens)
	tokenCounterCache.SetInt(outputTokenKey, newOutputTokens)
	fmt.Println("Token calculation completed successfully")
}

func inputText(messages []types.Message) string {
	allText := ""
	for _, message := range messages {
		allText += message.Text
	}
	return allText
}

// makeRequest formats the request and calls the chat/completions endpoint for code_completion requests
func (c *openAIChatCompletionStreamClient) makeRequest(ctx context.Context, requestParams types.CompletionRequestParameters, stream bool) (*http.Response, error) {
	if requestParams.TopK < 0 {
		requestParams.TopK = 0
	}
	if requestParams.TopP < 0 {
		requestParams.TopP = 0
	}

	// TODO(sqs): make CompletionRequestParameters non-anthropic-specific
	payload := openAIChatCompletionsRequestParameters{
		Model:       requestParams.Model,
		Temperature: requestParams.Temperature,
		TopP:        requestParams.TopP,
		// TODO(sqs): map requestParams.TopK to openai
		N:         1,
		Stream:    stream,
		MaxTokens: requestParams.MaxTokensToSample,
		// TODO: Our clients are currently heavily biased towards Anthropic,
		// so the stop sequences we send might not actually be very useful
		// for OpenAI.
		Stop: requestParams.StopSequences,
	}
	for _, m := range requestParams.Messages {
		// TODO(sqs): map these 'roles' to openai system/user/assistant
		var role string
		switch m.Speaker {
		case types.HUMAN_MESSAGE_SPEAKER:
			role = "user"
		case types.ASISSTANT_MESSAGE_SPEAKER:
			role = "assistant"
			//
		default:
			role = strings.ToLower(role)
		}
		payload.Messages = append(payload.Messages, message{
			Role:    role,
			Content: m.Text,
		})
	}

	reqBody, err := json.Marshal(payload)
	if err != nil {
		return nil, err
	}

	url, err := url.Parse(c.endpoint)
	if err != nil {
		return nil, errors.Wrap(err, "failed to parse configured endpoint")
	}
	url.Path = "v1/chat/completions"

	req, err := http.NewRequestWithContext(ctx, "POST", url.String(), bytes.NewReader(reqBody))
	if err != nil {
		return nil, err
	}

	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+c.accessToken)

	resp, err := c.cli.Do(req)
	if err != nil {
		return nil, err
	}

	if resp.StatusCode != http.StatusOK {
		return nil, types.NewErrStatusNotOK("OpenAI", resp)
	}

	return resp, nil
}

// makeCompletionRequest formats the request and calls the completions endpoint for code_completion requests
func (c *openAIChatCompletionStreamClient) makeCompletionRequest(ctx context.Context, requestParams types.CompletionRequestParameters, stream bool) (*http.Response, error) {
	if requestParams.TopK < 0 {
		requestParams.TopK = 0
	}
	if requestParams.TopP < 0 {
		requestParams.TopP = 0
	}

	prompt, err := getPrompt(requestParams.Messages)
	if err != nil {
		return nil, err
	}

	payload := openAICompletionsRequestParameters{
		Model:       requestParams.Model,
		Temperature: requestParams.Temperature,
		TopP:        requestParams.TopP,
		N:           1,
		Stream:      stream,
		MaxTokens:   requestParams.MaxTokensToSample,
		Stop:        requestParams.StopSequences,
		Prompt:      prompt,
	}

	reqBody, err := json.Marshal(payload)
	if err != nil {
		return nil, err
	}
	url, err := url.Parse(c.endpoint)
	if err != nil {
		return nil, errors.Wrap(err, "failed to parse configured endpoint")
	}
	url.Path = "v1/completions"

	req, err := http.NewRequestWithContext(ctx, "POST", url.String(), bytes.NewReader(reqBody))
	if err != nil {
		return nil, err
	}

	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+c.accessToken)

	resp, err := c.cli.Do(req)
	if err != nil {
		return nil, err
	}

	if resp.StatusCode != http.StatusOK {
		return nil, types.NewErrStatusNotOK("OpenAI", resp)
	}

	return resp, nil
}

// openAIChatCompletionsRequestParameters request object for openAI chat endpoint https://platform.openai.com/docs/api-reference/chat/create
type openAIChatCompletionsRequestParameters struct {
	Model            string             `json:"model"`                       // request.Model
	Messages         []message          `json:"messages"`                    // request.Messages
	Temperature      float32            `json:"temperature,omitempty"`       // request.Temperature
	TopP             float32            `json:"top_p,omitempty"`             // request.TopP
	N                int                `json:"n,omitempty"`                 // always 1
	Stream           bool               `json:"stream,omitempty"`            // request.Stream
	Stop             []string           `json:"stop,omitempty"`              // request.StopSequences
	MaxTokens        int                `json:"max_tokens,omitempty"`        // request.MaxTokensToSample
	PresencePenalty  float32            `json:"presence_penalty,omitempty"`  // unused
	FrequencyPenalty float32            `json:"frequency_penalty,omitempty"` // unused
	LogitBias        map[string]float32 `json:"logit_bias,omitempty"`        // unused
	User             string             `json:"user,omitempty"`              // unused
}

// openAICompletionsRequestParameters payload for openAI completions endpoint https://platform.openai.com/docs/api-reference/completions/create
type openAICompletionsRequestParameters struct {
	Model            string             `json:"model"`                       // request.Model
	Prompt           string             `json:"prompt"`                      // request.Messages[0] - formatted prompt expected to be the only message
	Temperature      float32            `json:"temperature,omitempty"`       // request.Temperature
	TopP             float32            `json:"top_p,omitempty"`             // request.TopP
	N                int                `json:"n,omitempty"`                 // always 1
	Stream           bool               `json:"stream,omitempty"`            // request.Stream
	Stop             []string           `json:"stop,omitempty"`              // request.StopSequences
	MaxTokens        int                `json:"max_tokens,omitempty"`        // request.MaxTokensToSample
	PresencePenalty  float32            `json:"presence_penalty,omitempty"`  // unused
	FrequencyPenalty float32            `json:"frequency_penalty,omitempty"` // unused
	LogitBias        map[string]float32 `json:"logit_bias,omitempty"`        // unused
	Suffix           string             `json:"suffix,omitempty"`            // unused
	User             string             `json:"user,omitempty"`              // unused
}

type message struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

type openaiUsage struct {
	PromptTokens     int `json:"prompt_tokens"`
	CompletionTokens int `json:"completion_tokens"`
	TotalTokens      int `json:"total_tokens"`
}

type openaiChoiceDelta struct {
	Content string `json:"content"`
}

type openaiChoice struct {
	Delta        openaiChoiceDelta `json:"delta"`
	Role         string            `json:"role"`
	Text         string            `json:"text"`
	FinishReason string            `json:"finish_reason"`
}

type openaiResponse struct {
	// Usage is only available for non-streaming requests.
	Usage   openaiUsage    `json:"usage"`
	Model   string         `json:"model"`
	Choices []openaiChoice `json:"choices"`
}

func getPrompt(messages []types.Message) (string, error) {
	if l := len(messages); l != 1 {
		return "", errors.Errorf("expected to receive exactly one message with the prompt (got %d)", l)
	}

	return messages[0].Text, nil
}
