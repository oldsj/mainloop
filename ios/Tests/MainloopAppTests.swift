import MainloopCore
import Testing
@testable import Mainloop

/// The app as built: the placeholder origin in Config/Mainloop.xcconfig means sample data.
@MainActor @Suite struct MainloopAppTests {
    @Test func placeholderBuildRunsOnSampleData() async {
        let environment = AppEnvironment.fromBundle()
        #expect(environment.isFixtures)
        #expect(environment.webOrigin == nil)
        await environment.needsYou.load()
        #expect(environment.needsYou.state == .loaded)
        #expect(environment.needsYou.count == 3)
    }
}
