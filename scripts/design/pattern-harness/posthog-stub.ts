// posthog-js hangs headless Chrome on file:// at import (core/paths -> auth store -> analytics); the harness sends no events.
const noop = () => undefined;
export default { init: noop, capture: noop, captureException: noop, identify: noop, register: noop, reset: noop, setPersonProperties: noop, people: { set: noop } };
