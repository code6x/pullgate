# Changelog

## [0.3.0](https://github.com/code6x/pullgate/compare/v0.2.0...v0.3.0) (2026-11-20)


### Features

* /resume and /ask-again from Telegram, limited so they cannot undo a safety stop ([#52](https://github.com/code6x/pullgate/issues/52)) ([b91914c](https://github.com/code6x/pullgate/commit/b91914c0f38b087742098850441c4ae9f259056d)), closes [#33](https://github.com/code6x/pullgate/issues/33)
* /status and /pause from Telegram ([#51](https://github.com/code6x/pullgate/issues/51)) ([9fa707e](https://github.com/code6x/pullgate/commit/9fa707e9156c36b1dcae1200c6c210eab74e7b70)), closes [#32](https://github.com/code6x/pullgate/issues/32)
* ask less often: settle time, a "nothing to apply" check exit, and one more question after a wait ([#53](https://github.com/code6x/pullgate/issues/53)) ([533f7c9](https://github.com/code6x/pullgate/commit/533f7c9c821fbc23674bd4547a6a25d9144c77a0))
* fix forward after a failed rollback instead of a pause that needs root ([#47](https://github.com/code6x/pullgate/issues/47)) ([91404b8](https://github.com/code6x/pullgate/commit/91404b8e121f5919fe419ece9303351b7749bd04)), closes [#34](https://github.com/code6x/pullgate/issues/34)
* pullgate bootstrap with pairing, so nobody types or pastes the owner ID ([#48](https://github.com/code6x/pullgate/issues/48)) ([4ab136a](https://github.com/code6x/pullgate/commit/4ab136ac4feb80ba3bd2bfb8443169ff3d150d25)), closes [#31](https://github.com/code6x/pullgate/issues/31)
* update pullgate from Telegram: a notice for each new release, /update installs it with its units ([#43](https://github.com/code6x/pullgate/issues/43)) ([8823788](https://github.com/code6x/pullgate/commit/8823788f4ce4d47413af387a2ce3bc6f08f857af)), closes [#9](https://github.com/code6x/pullgate/issues/9)
* WARN: lines of the summary stay in the head of the question, and apply can add short lines to its report ([#54](https://github.com/code6x/pullgate/issues/54)) ([47bf7cc](https://github.com/code6x/pullgate/commit/47bf7cc9db9f119c228cdf3655eb209320fe7a0c))

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
