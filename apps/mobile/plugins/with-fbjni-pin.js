const { withAppBuildGradle } = require("expo/config-plugins");

/**
 * Must equal the `fbjni` version in react-native's gradle/libs.versions.toml
 * (a test enforces this). react-android ships the libc++_shared.so built for
 * that fbjni; AndroidMath (via react-native-enriched-markdown) pulls fbjni
 * 0.8.1, whose libfbjni.so needs `__cxa_init_primary_exception` from a newer
 * libc++ than the one packaged, so the app crashes at SoLoader init.
 *
 * Plain CommonJS: Expo loads app.config.ts through plain `require`, which
 * cannot resolve an extensionless TypeScript import.
 */
const FBJNI_VERSION = "0.7.0";

const MARKER = "// fbjni-pin (CHE-2592)";

/** @param {string} contents */
function pinFbjni(contents) {
  if (contents.includes(MARKER)) return contents;
  return `${contents}
${MARKER}
configurations.all {
    resolutionStrategy {
        force 'com.facebook.fbjni:fbjni:${FBJNI_VERSION}'
    }
}
`;
}

/** @type {import("expo/config-plugins").ConfigPlugin} */
const withFbjniPin = (config) =>
  withAppBuildGradle(config, (c) => {
    c.modResults.contents = pinFbjni(c.modResults.contents);
    return c;
  });

module.exports = withFbjniPin;
module.exports.FBJNI_VERSION = FBJNI_VERSION;
module.exports.pinFbjni = pinFbjni;
