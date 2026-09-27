#include <cstddef>
#include <cstdint>
#include <iostream>
#include <vector>

// Touch every page so the working set stays resident and cannot be optimized
// away. The function is opaque to the caller, which keeps the allocation alive
// for the whole measured interval.
__attribute__((noinline)) void touchPages(std::vector<unsigned char>& buffer) {
  for (std::size_t i = 0; i < buffer.size(); i += 4096) {
    buffer[i] = static_cast<unsigned char>(i);
  }
}

int main() {
  std::uint64_t iterations = 0;
  if (!(std::cin >> iterations)) {
    return 1;
  }

  // Hold a fixed working set so peak memory is representative of a typical
  // accepted submission. Wave 1 reported about 44.9 MiB per submission at every
  // concurrency level; 44 MiB plus process overhead lands in that range.
  std::vector<unsigned char> buffer(44u * 1024u * 1024u, 0x5a);
  touchPages(buffer);

  std::uint64_t value = 0x9e3779b97f4a7c15ULL;
  for (std::uint64_t i = 0; i < iterations; ++i) {
    value ^= value << 13;
    value ^= value >> 7;
    value ^= value << 17;
    value += i ^ 0xd1b54a32d192ed03ULL;
  }
  std::cout << value << '\n';
  return 0;
}
