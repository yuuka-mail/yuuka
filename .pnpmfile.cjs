// openapi-typescript prints its output through the TypeScript 5 compiler
// API, which TypeScript 7 (the native port the WebUI type-checks with) no
// longer exports. Its peer dependency would bind it to the WebUI's
// TypeScript 7, so it gets a TypeScript 5 of its own instead; nothing
// else sees that copy. Drop this once openapi-typescript supports 7.
module.exports = {
  hooks: {
    readPackage(pkg) {
      if (pkg.name === "openapi-typescript") {
        delete pkg.peerDependencies?.typescript;
        pkg.dependencies = { ...pkg.dependencies, typescript: "5.9.3" };
      }
      return pkg;
    },
  },
};
