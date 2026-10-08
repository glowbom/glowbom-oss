package main

import (
	"encoding/json"
	"errors"
	"sync"
)

var sharedChatHistory = struct {
	sync.Mutex
	active    map[string]bool
	protected map[string][]string
}{active: map[string]bool{}, protected: map[string][]string{}}

var errSharedChatChanged = errors.New("This conversation changed on another device. Reopen the project to load its latest messages before saving.")

func chatMessageIdentity(message chatMessage) string {
	return chatSourceHash(message.Role + "\x00" + message.Text)
}

func readSharedChatHistory(path string) ([]chatMessage, error) {
	text, err := readChatProjectFile(path, ".glowbom/chat.json", maxChatHistoryBytes)
	if err != nil {
		return nil, err
	}
	messages := []chatMessage{}
	if text != "" && json.Unmarshal([]byte(text), &messages) != nil {
		return nil, errors.New("Could not read this saved conversation.")
	}
	if messages == nil {
		messages = []chatMessage{}
	}
	for _, message := range messages {
		if message.Role != "user" && message.Role != "assistant" {
			return nil, errors.New("Could not read this saved conversation.")
		}
	}
	return messages, nil
}

func saveSharedChatHistory(path string, messages []chatMessage) error {
	data, err := json.MarshalIndent(messages, "", "  ")
	if err != nil || len(data) > maxChatHistoryBytes {
		return errors.New("This conversation is too large to save. Start a new chat.")
	}
	directory, err := chatWriteDirectory(path, ".glowbom")
	if err != nil {
		return err
	}
	return atomicChatFile(directory, "chat.json", data)
}

func beginCompanionProjectChat(path, prompt string) ([]chatMessage, error) {
	sharedChatHistory.Lock()
	defer sharedChatHistory.Unlock()
	if sharedChatHistory.active[path] {
		return nil, errors.New("A reply is already running for this project. Wait for it to finish before sending another message.")
	}
	messages, err := readSharedChatHistory(path)
	if err != nil {
		return nil, err
	}
	// Retrying a failed or canceled turn keeps its already saved request.
	if len(messages) == 0 || messages[len(messages)-1].Role != "user" || messages[len(messages)-1].Text != prompt {
		message := chatMessage{Role: "user", Text: prompt}
		messages = append(messages, message)
		if err := saveSharedChatHistory(path, messages); err != nil {
			return nil, err
		}
		sharedChatHistory.protected[path] = append(sharedChatHistory.protected[path], chatMessageIdentity(message))
	}
	sharedChatHistory.active[path] = true
	return messages, nil
}

func endCompanionProjectChat(path string) {
	sharedChatHistory.Lock()
	delete(sharedChatHistory.active, path)
	sharedChatHistory.Unlock()
}

func finishCompanionProjectChat(path, text, model, reasoning, prompt string) ([]chatMessage, error) {
	sharedChatHistory.Lock()
	defer sharedChatHistory.Unlock()
	messages, err := readSharedChatHistory(path)
	if err != nil {
		return nil, err
	}
	if len(messages) == 0 || messages[len(messages)-1].Role != "user" || messages[len(messages)-1].Text != prompt {
		return nil, errSharedChatChanged
	}
	message := chatMessage{Role: "assistant", Text: text, Model: model, Reasoning: reasoning}
	messages = append(messages, message)
	if err := saveSharedChatHistory(path, messages); err != nil {
		return nil, err
	}
	sharedChatHistory.protected[path] = append(sharedChatHistory.protected[path], chatMessageIdentity(message))
	return messages, nil
}

// A stale full-history save cannot erase a phone request or reply, including
// after the stream has completed. The Desktop can resume saving after reloading
// the authoritative conversation. Message metadata may still be updated.
func validateSharedChatSave(path string, incoming []chatMessage) error {
	// The existing New conversation action sends an explicit empty history.
	// Preserve that action once the shared reply is no longer running.
	if len(incoming) == 0 && !sharedChatHistory.active[path] {
		return nil
	}
	protected := sharedChatHistory.protected[path]
	matched := 0
	for _, message := range incoming {
		if matched < len(protected) && chatMessageIdentity(message) == protected[matched] {
			matched++
		}
	}
	if matched != len(protected) {
		return errSharedChatChanged
	}
	if sharedChatHistory.active[path] {
		current, err := readSharedChatHistory(path)
		if err != nil || len(current) != len(incoming) {
			return errSharedChatChanged
		}
		for i := range current {
			if chatMessageIdentity(current[i]) != chatMessageIdentity(incoming[i]) {
				return errSharedChatChanged
			}
		}
	}
	return nil
}
