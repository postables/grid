package grid

import (
	"encoding/gob"
	"errors"
	"fmt"
	"hash/fnv"
)

var (
	ErrReceiverBusy          = errors.New("receiver busy")
	ErrUnknownMailbox        = errors.New("unknown mailbox")
	ErrContextFinished       = errors.New("context finished")
	ErrInvalidActorType      = errors.New("invalid actor type")
	ErrInvalidActorName      = errors.New("invalid actor name")
	ErrInvalidActorNamespace = errors.New("invalid actor namespace")
)

// NewActorDef with given namespace and name. The caller
// should set the Type, the default is to make it the
// same as the name.
func NewActorDef(name string) *ActorDef {
	return &ActorDef{
		Type: name,
		Name: name,
	}
}

// ActorDef defines the namespace, name, and type of an actor.
type ActorDef struct {
	id        string
	Type      string
	Name      string
	namespace string
}

// ID of the actor, in format of <namespace> . <name> but without whitespace.
func (a *ActorDef) ID() string {
	if a.id == "" {
		a.id = fmt.Sprintf("%v-%v", a.namespace, a.Name)
	}
	return a.id
}

func (a *ActorDef) regID() string {
	h := fnv.New64()
	_, err := h.Write([]byte(a.ID()))
	if err != nil {
		panic("failed hashing actor id")
	}
	return fmt.Sprintf("%v-%v", a.ID(), h.Sum64())
}

// ValidateActorDef components, such as name and namespace.
func ValidateActorDef(def *ActorDef) error {
	if !isNameValid(def.Type) {
		return ErrInvalidActorType
	}
	if !isNameValid(def.Name) {
		return ErrInvalidActorName
	}
	if !isNameValid(def.namespace) {
		return ErrInvalidActorNamespace
	}
	return nil
}

// ResponseMsg for generic response that need only a
// success flag or error.
type ResponseMsg struct {
	Succeeded bool
	Error     string
}

// Err retured in response if any, nil otherwise.
func (m *ResponseMsg) Err() error {
	if !m.Succeeded && m.Error != "" {
		return errors.New(m.Error)
	}
	return nil
}

func init() {
	gob.Register(&ResponseMsg{})
	gob.Register(&ActorDef{})
}
