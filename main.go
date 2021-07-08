package main

import (
	"fmt"
	"log"
	"os"
	"os/exec"
	"regexp"
	"strings"

	"github.com/foxcpp/go-assuan/common"
	"github.com/foxcpp/go-assuan/pinentry"
	pinentryBinary "github.com/gopasspw/pinentry"
	"github.com/keybase/go-keychain"
	touchid "github.com/lox/go-touchid"
)

var (
	emailRegex = regexp.MustCompile(`\"(?P<name>.*<(?P<email>.*)>)\"`)
	keyIDRegex = regexp.MustCompile(`ID (?P<keyId>.*),`)
	// keyID should be of exactly 8 or 16 characters
)

const (
	DefaultLogLocation = "/tmp/test.log"
	DefaultLoggerFlags = log.Ldate | log.Ltime | log.Lshortfile
)

// checkEntryInKeychain executes a search in the current keychain. The search configured to not
// return the Data stored in the Keychain, as a result this should not require any type of
// authentication.
func checkEntryInKeychain(label string) (bool, error) {
	query := keychain.NewItem()
	query.SetSecClass(keychain.SecClassGenericPassword)
	query.SetLabel(label)
	query.SetMatchLimit(keychain.MatchLimitOne)
	query.SetReturnData(false)
	query.SetReturnAttributes(true)

	results, err := keychain.QueryItem(query)
	if err != nil {
		return false, err
	}

	return len(results) == 1, nil
}

// KeychainClient represents a single instance of a pinentry server
type KeychainClient struct {
	logger *log.Logger
}

func New() KeychainClient {
	var logger *log.Logger

	if _, err := os.Stat(DefaultLogLocation); os.IsNotExist(err) {
		file, err := os.Create(DefaultLogLocation)
		if err != nil {
			panic("Couldn't create log file")
		}

		logger = log.New(file, "", DefaultLoggerFlags)
	} else {
		// append to the existing log file
		file, err := os.OpenFile(DefaultLogLocation, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0666)
		if err != nil {
			panic(err)
		}

		logger = log.New(file, "", DefaultLoggerFlags)
	}

	return KeychainClient{
		logger: logger,
	}
}

func WithLogger(logger *log.Logger) KeychainClient {
	return KeychainClient{
		logger: logger,
	}
}

// passwordFromKeychain retrieves a password given a label from the Keychain
func passwordFromKeychain(label string) (string, error) {
	query := keychain.NewItem()
	query.SetSecClass(keychain.SecClassGenericPassword)
	query.SetLabel(label)
	query.SetMatchLimit(keychain.MatchLimitOne)
	query.SetReturnData(true)

	results, err := keychain.QueryItem(query)
	if err != nil {
		return "", err
	}

	if len(results) > 1 {
		return "", fmt.Errorf("multiple passwords matched the query")
	}

	return string(results[0].Data), nil
}

// storePasswordInKeychain saves a password/pin in the keychain with the given label
// and keyInfo
func storePasswordInKeychain(label, keyInfo string, pin []byte) error {
	item := keychain.NewItem()
	item.SetSecClass(keychain.SecClassGenericPassword)
	item.SetService("GnuPG")
	item.SetAccount(keyInfo)
	item.SetLabel(label)
	item.SetData(pin)
	item.SetSynchronizable(keychain.SynchronizableNo)
	item.SetAccessible(keychain.AccessibleWhenUnlocked)

	if err := keychain.AddItem(item); err != nil {
		return err
	}

	return nil
}

// askForPassword uses the default pinentry-mac program for getting the password from the user
func askForPassword(s pinentry.Settings) ([]byte, error) {
	p, err := pinentryBinary.New()
	if err != nil {
		return []byte{}, fmt.Errorf("failed to start %q: %w", pinentryBinary.GetBinary(), err)
	}
	defer p.Close()

	p.Set("title", "pinentry-mac-touchid PIN Prompt")

	// passthrough the original description that its used for creating the keychain item
	p.Set("desc", strings.ReplaceAll(s.Desc, "\n", "\\n"))
	p.Set("prompt", "Please enter your PIN:")

	// Enable opt-in external PIN caching (in the OS keychain).
	// https://gist.github.com/mdeguzis/05d1f284f931223624834788da045c65#file-info-pinentry-L324
	//
	// Ideally if this option was not set, pinentry-mac should hide the `Save in Keychain`
	// checkbox, but this is not the case.
	// p.Option("allow-external-password-cache")
	p.Set("KEYINFO", s.KeyInfo)

	return p.GetPin()
}

type AuthFunc func(reason string) (bool, error)
type GetPinFunc func(pinentry.Settings) (string, *common.Error)

func GetPIN(fn AuthFunc, logger *log.Logger) GetPinFunc {
	return func(s pinentry.Settings) (string, *common.Error) {
		matches := emailRegex.FindStringSubmatch(s.Desc)
		name := strings.Split(matches[1], " <")[0]
		email := matches[2]

		matches = keyIDRegex.FindStringSubmatch(s.Desc)
		keyID := matches[1]
		if len(keyID) != 8 && len(keyID) != 16 {
			logger.Fatalf("Invalid keyID: %s", keyID)
		}

		keychainLabel := fmt.Sprintf("%s <%s> (%s)", name, email, keyID)
		exists, err := checkEntryInKeychain(keychainLabel)
		if err != nil {
			logger.Fatalf("error checking entry in keychain: %s", err)
		}

		// If the entry is not found in the keychain, we trigger `pinentry-mac` with the option
		// to save the pin in the keychain.
		//
		// When trying to access the newly created keychain item we will get the normal password prompt
		// from the OS, we need to "Always allow" access to our application, still the access from our
		// app to the keychain item will be guarded by Touch ID.
		//
		// Currently I'm not aware of a way for automatically adding our binary to the list of always
		// allowed apps, see: https://github.com/keybase/go-keychain/issues/54.
		if !exists {
			pin, err := askForPassword(s)
			if err != nil {
				logger.Printf("Error calling pinentry-mac: %s", err)
			}

			if len(pin) == 0 {
				logger.Fatalf("pinentry-mac didn't return a password")
			}

			// s.KeyInfo is always in the form of x/cacheId
			// https://gist.github.com/mdeguzis/05d1f284f931223624834788da045c65#file-info-pinentry-L357-L362
			keyInfo := strings.Split(s.KeyInfo, "/")[1]

			// pinentry-mac can create an item in the keychain, if that was the case, the user will have
			// to authorize our app to access the item without asking for a password from the user. If
			// not, we create an entry in the keychain, which automatically gives us ownership (i.e the
			// user will not be asked for a password). In either case, the access to the item will be
			// guarded by Touch ID.
			exists, err = checkEntryInKeychain(keychainLabel)
			if err != nil {
				logger.Fatalf("error checking entry in keychain: %s", err)
			}

			if !exists {
				// pinentry-mac didn't create a new entry in the keychain, we create our own and take
				// ownership over the entry.
				err = storePasswordInKeychain(keychainLabel, keyInfo, pin)

				if err == keychain.ErrorDuplicateItem {
					logger.Fatalf("Duplicated entry in the keychain")
				}
			} else {
				logger.Printf("The keychain entry was created by pinentry-mac. Permission will be required on next run.")
			}

			return string(pin), nil
		}

		var ok bool
		if ok, err = fn(fmt.Sprintf("access the PIN for %s", keychainLabel)); err != nil {
			logger.Fatalf("Error authenticating with Touch ID: %s", err)

			return "", nil
		}

		if !ok {
			logger.Printf("Failed to authenticate")
			return "", nil
		}

		password, err := passwordFromKeychain(keychainLabel)
		if err != nil {
			log.Printf("Error fetching password from Keychain %s", err)
		}

		return password, nil
	}
}

func Confirm(pinentry.Settings) (bool, *common.Error) {
	fmt.Println("Confirm was called!")

	return true, nil
}

func Msg(pinentry.Settings) *common.Error {
	fmt.Println("Msg was called!")

	return nil
}

func main() {
	var logger *log.Logger
	if _, err := os.Stat(DefaultLogLocation); os.IsNotExist(err) {
		file, err1 := os.Create(DefaultLogLocation)
		if err1 != nil {
			panic(err1)
		}
		// new file if it doesn't exist
		logger = log.New(file, "", log.Ldate|log.Ltime|log.Lshortfile)
	} else {
		// append to the existing log file
		file, err := os.OpenFile(DefaultLogLocation, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0666)
		if err != nil {
			panic(err)
		}
		logger = log.New(file, "", log.Ldate|log.Ltime|log.Lshortfile)
	}

	logger.Println("Ready!")

	if _, err := exec.LookPath(pinentryBinary.GetBinary()); err != nil {
		log.Fatalf("PIN entry program %q not found!", pinentryBinary.GetBinary())
	}

	callbacks := pinentry.Callbacks{
		GetPIN: func(s pinentry.Settings) (string, *common.Error) {
			return GetPIN(func(reason string) (bool, error) {
				return touchid.Authenticate(reason)
			}, logger)(s)
		},
		Confirm: Confirm,
		Msg:     Msg,
	}

	pinentry.Serve(callbacks, "Hi from pinentry-mac-touchid!")
}