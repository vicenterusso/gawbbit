package pyval

// Error carries the Python exception class the original implementation raised for a
// malformed event. Only the class name is ever logged, never event content.
type Error struct{ Class string }

func (e *Error) Error() string { return e.Class }

// ErrorClass is the Python exception class name.
func (e *Error) ErrorClass() string { return e.Class }

// Exception classes the transformer can raise.
var (
	ErrAttribute     = &Error{Class: "AttributeError"}
	ErrOverflow      = &Error{Class: "OverflowError"}
	ErrUnicodeEncode = &Error{Class: "UnicodeEncodeError"}
	ErrNotJSMessage  = &Error{Class: "NotJSMessageError"}
)
