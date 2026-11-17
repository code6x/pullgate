# Changelog

## [0.2.0](https://github.com/code6x/pullgate/compare/v0.1.1...v0.2.0) (2026-11-17)


### Features

* /help in Telegram, and a hint for messages the bot does not handle ([#38](https://github.com/code6x/pullgate/issues/38)) ([c341ec1](https://github.com/code6x/pullgate/commit/c341ec1441f01f9aee08954d996c08d6a0d80adf)), closes [#26](https://github.com/code6x/pullgate/issues/26)


### Bug Fixes

* --ask-again closes a lost open question, and the listener reports a bad outbox file ([#35](https://github.com/code6x/pullgate/issues/35)) ([38ecde6](https://github.com/code6x/pullgate/commit/38ecde6bc5fb2869f1ec49f42d39f1206de4adb8)), closes [#22](https://github.com/code6x/pullgate/issues/22)
* a failed check notice shows the end of the output, with short tree paths ([#39](https://github.com/code6x/pullgate/issues/39)) ([a7b20af](https://github.com/code6x/pullgate/commit/a7b20afcf01f5558a2a821f11e7745ff37b9fc8d)), closes [#25](https://github.com/code6x/pullgate/issues/25)
* all text from the repo is shown only as code, and SHAs are short ([#37](https://github.com/code6x/pullgate/issues/37)) ([44afa8b](https://github.com/code6x/pullgate/commit/44afa8b6da2aa96e25aa6e6aef05d91c65395003)), closes [#29](https://github.com/code6x/pullgate/issues/29) [#23](https://github.com/code6x/pullgate/issues/23) [#30](https://github.com/code6x/pullgate/issues/30)

## [0.1.1](https://github.com/code6x/pullgate/compare/v0.1.0...v0.1.1) (2026-11-16)


### Bug Fixes

* accept --version and -version as a global flag ([#15](https://github.com/code6x/pullgate/issues/15)) ([5450bbc](https://github.com/code6x/pullgate/commit/5450bbc7683c3ad19a7969ed566540d1e1fa2254))
* pullgate-listen.service gets an [Install] section ([#24](https://github.com/code6x/pullgate/issues/24)) ([053d0aa](https://github.com/code6x/pullgate/commit/053d0aae8208822b7e5c4d2d6f144c5ca68e1bc8))
* the first run works under RestrictSUIDSGID=yes (no setgid outbox) ([#21](https://github.com/code6x/pullgate/issues/21)) ([1236625](https://github.com/code6x/pullgate/commit/12366256c83e6ef0d51511f82c78417f680a890d))

## 0.1.0 (2026-11-12)


### Features

* /check from Telegram to look for a new commit now ([#13](https://github.com/code6x/pullgate/issues/13)) ([fbd8782](https://github.com/code6x/pullgate/commit/fbd8782a7d63b9d18d772dd5fa53b9a15d5f711e))
* install.sh that installs or updates pullgate from a checked release ([#10](https://github.com/code6x/pullgate/issues/10)) ([dd7638d](https://github.com/code6x/pullgate/commit/dd7638d2f2e23c5c081146fb37824b7837c58c49))
* run, listen, apply and status ([#3](https://github.com/code6x/pullgate/issues/3)) ([d5e71ec](https://github.com/code6x/pullgate/commit/d5e71ece0833d859cd4f2818bf8c2bcef7bc6296))


### Documentation

* design, threat model, rules and CI ([#2](https://github.com/code6x/pullgate/issues/2)) ([2bc846d](https://github.com/code6x/pullgate/commit/2bc846d31dd50e302b27b75ee2f05a418e67bd51))
* releasing.md stands alone and asks for no required reviewer ([#12](https://github.com/code6x/pullgate/issues/12)) ([8cc4e3a](https://github.com/code6x/pullgate/commit/8cc4e3aa2740f2ff1652adc601c768ca2729d452))
