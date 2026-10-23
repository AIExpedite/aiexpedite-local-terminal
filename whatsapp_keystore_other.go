//go:build !windows && !darwin && !linux

package main

import "errors"

// No supported credential store on this OS: WhatsApp linking is refused with
// WHATSAPP_SECURE_STORAGE_UNAVAILABLE.
type platformWhatsAppKeystore struct{}

var errWhatsAppNoKeystore = errors.New("no OS credential store")

func (platformWhatsAppKeystore) Available() bool                 { return false }
func (platformWhatsAppKeystore) Put(string, []byte) error        { return errWhatsAppNoKeystore }
func (platformWhatsAppKeystore) Get(string) ([]byte, error)      { return nil, errWhatsAppNoKeystore }
func (platformWhatsAppKeystore) Delete(string) error             { return errWhatsAppNoKeystore }
