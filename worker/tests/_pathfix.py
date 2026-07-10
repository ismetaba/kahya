"""Ensures the repo's `worker/` directory (the parent of this `tests/`
directory) is on `sys.path`, so `import kahya_worker` resolves.

`make test` runs exactly `python -m unittest discover -s worker/tests`
(no `-t/--top-level-directory`), so `unittest.loader.TestLoader.discover`
only ever inserts `worker/tests` itself onto `sys.path` - never its
parent (`worker/`, where the `kahya_worker` package actually lives). Every
test module in this directory that imports anything from `kahya_worker`
must `import _pathfix` first (this module is a plain sibling file, so it
resolves via the `worker/tests` sys.path entry unittest's own discovery
already adds - no further bootstrapping needed for THIS import to work).
"""

import os
import sys

_WORKER_DIR = os.path.dirname(os.path.dirname(os.path.abspath(__file__)))
if _WORKER_DIR not in sys.path:
    sys.path.insert(0, _WORKER_DIR)
