// sessions are the higher level ChatGPT like UI concept

package controller

import (
	"context"
	"fmt"

	"github.com/davecgh/go-spew/spew"
	"github.com/lukemarsden/helix/api/pkg/model"
	"github.com/lukemarsden/helix/api/pkg/store"
	"github.com/lukemarsden/helix/api/pkg/types"
)

// set to false in production (will log messages to web UI)
const DEBUG = true

// the core function - decide which task to give to a worker
// TODO: keep track of the previous tasks run by this worker (and therefore we know which weights are loaded into RAM)
// try to send similar tasks to the same worker
func (c *Controller) ShiftSessionQueue(ctx context.Context, filter types.SessionFilter) (*types.Session, error) {
	c.sessionQueueMtx.Lock()
	defer c.sessionQueueMtx.Unlock()

	// right now this is very dumb - it literally just returns the next thing and doesn't even care what type it is
	// TODO: get the worker auth system plugged in so we know who is asking for the task
	// and then we can keep track of the last thing they ran and pick better
	for i, session := range c.sessionQueue {
		if filter.Mode != "" && session.Mode != filter.Mode {
			continue
		}
		if filter.Type != "" && session.Type != filter.Type {
			continue
		}
		if filter.ModelName != "" && session.ModelName != filter.ModelName {
			continue
		}
		c.sessionQueue = append(c.sessionQueue[:i], c.sessionQueue[i+1:]...)
		return session, nil
	}

	return nil, nil
}

func (c *Controller) ConvertSessionToTask(ctx context.Context, session *types.Session) (*types.WorkerTask, error) {
	if session == nil {
		return nil, nil
	}

	task := &types.WorkerTask{
		SessionID: session.ID,
		Session:   *session,
	}

	switch {
	case session.Mode == "Create" && session.Type == "Text":
		model, err := model.GetLanguageModel(session.ModelName)
		if err != nil {
			return nil, err
		}
		prompt, err := model.GetPrompt(ctx, session)
		if err != nil {
			return nil, err
		}
		task.Prompt = prompt
		return task, nil
	case session.Mode == "Create" && session.Type == "Image":
		model, err := model.GetImageModel(session.ModelName)
		if err != nil {
			return nil, err
		}
		prompt, err := model.GetPrompt(ctx, session)
		if err != nil {
			return nil, err
		}
		task.Prompt = prompt
		return task, nil
	case session.Mode == "Finetune" && session.Type == "Text":
		return nil, nil
	case session.Mode == "Finetune" && session.Type == "Image":
		return nil, nil
	}
	return nil, nil
}

// add the given session onto the end of the queue
// unless it's already waiting and present in the queue
// in which case let's replace it at it's current position
func (c *Controller) PushSessionQueue(ctx context.Context, session *types.Session) error {
	c.sessionQueueMtx.Lock()
	defer c.sessionQueueMtx.Unlock()

	existing := false
	newQueue := []*types.Session{}
	for _, existingSession := range c.sessionQueue {
		if existingSession.ID == session.ID {
			newQueue = append(newQueue, session)
			existing = true
		} else {
			newQueue = append(newQueue, existingSession)
		}
	}
	if !existing {
		newQueue = append(newQueue, session)
	}

	c.sessionQueue = newQueue
	return nil
}

func (c *Controller) AddActiveSession(ctx context.Context, session *types.Session) error {
	c.activeSessionMtx.Lock()
	defer c.activeSessionMtx.Unlock()

	c.activeSessions[session.ID] = session

	// spawn a new text stream to listen in for responses
	if session.Type == "Text" && session.Mode == "Create" {
		sessionModel, err := model.GetLanguageModel(session.ModelName)
		if err != nil {
			return err
		}

		// this knows how to parse the output of the model
		textStream, err := sessionModel.GetTextStream(ctx)
		if err != nil {
			return err
		}

		c.activeTextStreamsMtx.Lock()
		defer c.activeTextStreamsMtx.Unlock()
		c.activeTextStreams[session.ID] = textStream

		go textStream.Start(ctx)

		// // this is what will listen to the text stream and send messages to the
		// // database and the websockets
		// go func() {
		// 	for {
		// 		select {
		// 		case msg := <-textStream.Output:
		// 			func() {
		// 				c.activeSessionMtx.Lock()
		// 				defer c.activeSessionMtx.Unlock()

		// 				msgs := session.Interactions.Messages
		// 				latest := msgs[len(msgs)-1]
		// 				latest.Message += msg
		// 				msgs[len(msgs)-1] = latest
		// 				session.Interactions.Messages = msgs

		// 				_, err := c.Options.Store.UpdateSession(ctx, *session)
		// 				if err != nil {
		// 					log.Printf("Error adding message: %s", err)
		// 				}

		// 				c.SessionUpdatesChan <- session
		// 			}()
		// 			fmt.Print("Got message from text stream: ", msg)
		// 		}
		// 	}
		// }()
	}
	return nil
}

func (c *Controller) GetActiveSession(ctx context.Context, id string) (*types.Session, error) {
	c.activeSessionMtx.Lock()
	defer c.activeSessionMtx.Unlock()
	session, ok := c.activeSessions[id]
	if !ok {
		return nil, fmt.Errorf("session not found")
	}
	return session, nil
}

func (c *Controller) GetActiveTextStream(ctx context.Context, id string) (*model.TextStream, error) {
	c.activeTextStreamsMtx.Lock()
	defer c.activeTextStreamsMtx.Unlock()
	textStream, ok := c.activeTextStreams[id]
	if !ok {
		return nil, fmt.Errorf("text stream not found")
	}
	return textStream, nil
}

func (c *Controller) RemoveActiveSession(ctx context.Context, id string) error {
	c.activeSessionMtx.Lock()
	defer c.activeSessionMtx.Unlock()
	if _, ok := c.activeSessions[id]; !ok {
		return fmt.Errorf("session not found")
	}
	delete(c.activeSessions, id)
	return nil
}

func (c *Controller) RemoveActiveTextStream(ctx context.Context, id string) error {
	c.activeTextStreamsMtx.Lock()
	defer c.activeTextStreamsMtx.Unlock()
	if _, ok := c.activeTextStreams[id]; !ok {
		return fmt.Errorf("text stream not found")
	}
	delete(c.activeTextStreams, id)
	return nil
}

// if the action is "begin" - then we need to ceate a new textstream that is hooked up correctly
// then we stash that in a map
// if the action is "continue" - load the textstream and write to it
// if the action is "end" - unload the text stream
func (c *Controller) HandleWorkerResponse(ctx context.Context, taskResponse *types.WorkerTaskResponse) (*types.WorkerTaskResponse, error) {
	session, err := c.GetActiveSession(ctx, taskResponse.SessionID)
	if err != nil {
		return nil, err
	}

	switch {
	case session.Mode == "Create" && session.Type == "Text":
		return c.handleWorkerResponseLanguageInference(ctx, taskResponse, session)
	case session.Mode == "Create" && session.Type == "Image":
		return c.handleWorkerResponseImageInference(ctx, taskResponse, session)
	case session.Mode == "Finetune" && session.Type == "Text":
		return nil, nil
	case session.Mode == "Finetune" && session.Type == "Image":
		return nil, nil
	}
	return nil, nil
}

func (c *Controller) handleWorkerResponseLanguageInference(ctx context.Context, taskResponse *types.WorkerTaskResponse, session *types.Session) (*types.WorkerTaskResponse, error) {
	// if taskResponse.Action == types.WorkerTaskResponseActionStreamOpen {
	// 	session.Interactions = append(session.Interactions, types.Interaction{
	// 		Creator:  types.MessageCreatorSystem,
	// 		Message:  taskResponse.Chunk,
	// 		Uploads:  []string{}, // cool, computer can create images here
	// 		Finished: false,
	// 	})
	// 	_, err := c.Options.Store.UpdateSession(ctx, *session)
	// 	if err != nil {
	// 		return nil, err
	// 	}
	// 	c.SessionUpdatesChan <- session
	// 	return taskResponse, nil
	// } else if taskResponse.Action == types.WorkerTaskResponseActionStreamContinue {
	// 	textStream, err := c.GetActiveTextStream(ctx, taskResponse.SessionID)
	// 	if err != nil {
	// 		return nil, err
	// 	}
	// 	textStream.Write([]byte(taskResponse.Message))
	// 	return taskResponse, nil
	// } else if taskResponse.Action == types.WorkerTaskResponseActionEnd {
	// 	textStream, err := c.GetActiveTextStream(ctx, taskResponse.SessionID)
	// 	if err != nil {
	// 		return nil, err
	// 	}
	// 	err = textStream.Close(ctx)
	// 	if err != nil {
	// 		return nil, err
	// 	}
	// 	err = c.RemoveActiveTextStream(ctx, taskResponse.SessionID)
	// 	if err != nil {
	// 		return nil, err
	// 	}
	// 	return taskResponse, nil
	// } else {
	// 	return nil, nil
	// }
	return nil, nil
}

func (c *Controller) handleWorkerResponseImageInference(ctx context.Context, taskResponse *types.WorkerTaskResponse, session *types.Session) (*types.WorkerTaskResponse, error) {
	fmt.Printf(" --------------------------------------\n")
	spew.Dump(taskResponse)
	return taskResponse, nil
}

// load the session queues from the database in case of restart
func (c *Controller) loadSessionQueues(ctx context.Context) error {
	c.sessionQueueMtx.Lock()
	defer c.sessionQueueMtx.Unlock()

	sessionQueue := []*types.Session{}

	st := c.Options.Store

	// fetch all sessions - this is in DESC order so we need to reverse the array
	sessions, err := st.GetSessions(ctx, store.GetSessionsQuery{})
	if err != nil {
		return err
	}

	for i := len(sessions) - 1; i >= 0; i-- {
		session := sessions[i]

		interactions := session.Interactions
		if interactions == nil || len(interactions) == 0 {
			// should never happen, sessions are always initiated by the user
			// creating an initial message
			continue
		}

		latest := interactions[len(interactions)-1]
		if latest.Creator == types.CreatorTypeSystem {
			// we've already given a response, don't need to do anything
			continue
		}

		sessionQueue = append(sessionQueue, session)
	}

	// now we have the queue in oldest first order
	c.sessionQueue = sessionQueue
	return nil
}
