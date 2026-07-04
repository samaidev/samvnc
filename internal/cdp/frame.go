package cdp

import (
        "context"
        "encoding/json"
        "fmt"
        "strings"
)

// EvalInFrame evaluates JavaScript in a specific frame by URL substring.
// Uses Page.getFrameTree to find the frame, then Runtime.evaluate with
// the frame's execution context.
func (c *Client) EvalInFrame(ctx context.Context, urlContains, expression string) (interface{}, error) {
        // 1. Enable Page domain to get frame tree
        _, err := c.send(ctx, "Page.enable", nil)
        if err != nil {
                return nil, fmt.Errorf("cdp: Page.enable: %w", err)
        }

        // 2. Get frame tree
        result, err := c.send(ctx, "Page.getFrameTree", nil)
        if err != nil {
                return nil, fmt.Errorf("cdp: Page.getFrameTree: %w", err)
        }

        var frameTree struct {
                FrameTree struct {
                        Frame struct {
                                ID       string `json:"id"`
                                URL      string `json:"url"`
                                Name     string `json:"name"`
                        } `json:"frame"`
                        ChildFrames []struct {
                                Frame struct {
                                        ID   string `json:"id"`
                                        URL  string `json:"url"`
                                        Name string `json:"name"`
                                } `json:"frame"`
                        } `json:"childFrames"`
                } `json:"frameTree"`
        }
        if err := json.Unmarshal(result, &frameTree); err != nil {
                return nil, fmt.Errorf("cdp: parse frame tree: %w", err)
        }

        // 3. Find the frame matching urlContains
        var frameID string
        for _, cf := range frameTree.FrameTree.ChildFrames {
                if strings.Contains(cf.Frame.URL, urlContains) {
                        frameID = cf.Frame.ID
                        break
                }
        }
        if frameID == "" {
                // Also check nested children
                return nil, fmt.Errorf("cdp: no frame matching %q (checked %d child frames)", urlContains, len(frameTree.FrameTree.ChildFrames))
        }

        // 4. Enable Runtime to get execution contexts
        _, err = c.send(ctx, "Runtime.enable", nil)
        if err != nil {
                return nil, fmt.Errorf("cdp: Runtime.enable: %w", err)
        }

        // 5. Get all execution contexts
        // We need to listen for Runtime.executionContextCreated events
        // But since we can't easily do that with our simple client, 
        // let's use a different approach: create an isolated world for the frame

        // Actually, the simplest way is to use Runtime.evaluate with contextId.
        // But we need to know the contextId for the frame.
        // Let's use Page.createIsolatedWorld instead.

        createWorldParams := map[string]interface{}{
                "frameId": frameID,
        }
        createWorldResult, err := c.send(ctx, "Page.createIsolatedWorld", createWorldParams)
        if err != nil {
                return nil, fmt.Errorf("cdp: createIsolatedWorld: %w", err)
        }

        var createWorldResp struct {
                ExecutionContextID int64 `json:"executionContextId"`
        }
        if err := json.Unmarshal(createWorldResult, &createWorldResp); err != nil {
                return nil, fmt.Errorf("cdp: parse isolated world: %w", err)
        }

        // 6. Evaluate JS in the isolated world (which is in the frame's context)
        evalParams := map[string]interface{}{
                "expression":    expression,
                "returnByValue": true,
                "awaitPromise":  true,
                "contextId":     createWorldResp.ExecutionContextID,
        }
        evalResult, err := c.send(ctx, "Runtime.evaluate", evalParams)
        if err != nil {
                return nil, fmt.Errorf("cdp: frame eval: %w", err)
        }

        var evalResp struct {
                Result struct {
                        Type  string          `json:"type"`
                        Value json.RawMessage `json:"value"`
                } `json:"result"`
                ExceptionDetails *struct {
                        Text string `json:"text"`
                } `json:"exceptionDetails"`
        }
        if err := json.Unmarshal(evalResult, &evalResp); err != nil {
                return nil, fmt.Errorf("cdp: parse eval result: %w", err)
        }
        if evalResp.ExceptionDetails != nil {
                return nil, fmt.Errorf("cdp: frame JS exception: %s", evalResp.ExceptionDetails.Text)
        }

        var value interface{}
        if err := json.Unmarshal(evalResp.Result.Value, &value); err != nil {
                return string(evalResp.Result.Value), nil
        }
        return value, nil
}
