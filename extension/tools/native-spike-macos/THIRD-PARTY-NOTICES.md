# Native spike dependencies

AXorcist is linked through Swift Package Manager at the revision in
`Package.swift`. Its MIT license and transitive dependency licenses must accompany
any distributed helper. This development spike does not distribute a binary.

The experimental PID/window event field mapping in `main.swift` adapts
`post_mouse_event_with_mode` from trycua/cua, revision
`9bbfa7dd3e27ca7f1861ede70aaca390174493f9`,
`libs/cua-driver/rust/crates/platform-macos/src/input/mouse.rs`.
`CuaWindowClick.swift` also adapts `click_at_xy_chromium` and the dynamic
symbol bindings in `skylight.rs`. It evaluates the SkyLight/window event stream
and Chromium primer, while omitting activation and focus changes. This is a
separately measured subset, not a claim of full upstream behavior.

MIT License

Copyright (c) 2025 Cua AI, Inc.

Permission is hereby granted, free of charge, to any person obtaining a copy
of this software and associated documentation files (the "Software"), to deal
in the Software without restriction, including without limitation the rights
to use, copy, modify, merge, publish, distribute, sublicense, and/or sell
copies of the Software, and to permit persons to whom the Software is
furnished to do so, subject to the following conditions:

The above copyright notice and this permission notice shall be included in all
copies or substantial portions of the Software.

THE SOFTWARE IS PROVIDED "AS IS", WITHOUT WARRANTY OF ANY KIND, EXPRESS OR
IMPLIED, INCLUDING BUT NOT LIMITED TO THE WARRANTIES OF MERCHANTABILITY,
FITNESS FOR A PARTICULAR PURPOSE AND NONINFRINGEMENT. IN NO EVENT SHALL THE
AUTHORS OR COPYRIGHT HOLDERS BE LIABLE FOR ANY CLAIM, DAMAGES OR OTHER
LIABILITY, WHETHER IN AN ACTION OF CONTRACT, TORT OR OTHERWISE, ARISING FROM,
OUT OF OR IN CONNECTION WITH THE SOFTWARE OR THE USE OR OTHER DEALINGS IN THE
SOFTWARE.
