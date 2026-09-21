package openaiapi

// Model is one entry of the OpenAI Models API as Halro serves it: the id is a
// public alias the caller's Project may name, and nothing about the upstream
// behind it is disclosed.
type Model struct {
	ID      string `json:"id"`
	Object  string `json:"object"`
	Created int64  `json:"created"`
	OwnedBy string `json:"owned_by"`
}

type ModelList struct {
	Object string  `json:"object"`
	Data   []Model `json:"data"`
}
