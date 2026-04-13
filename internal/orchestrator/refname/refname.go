// Package refname generates random "adjective_noun" style names for task refs.
// Names use a curated list of adjectives and scientist/mathematician/engineer names.
package refname

import "math/rand/v2"

// adjectives is the list of adjectives used in name generation.
var adjectives = []string{
	"abstract", "accurate", "adept", "agile", "alert", "ardent", "astute", "attentive",
	"balanced", "bold", "brave", "bright", "brisk",
	"calm", "capable", "careful", "classic", "clean", "clear", "clever", "cool", "crisp", "curious",
	"daring", "decent", "deft", "diligent", "dynamic",
	"eager", "earnest", "elastic", "elegant", "elated", "elite", "energetic",
	"faithful", "fancy", "fast", "firm", "fluid", "fluent", "focused", "frugal",
	"genuine", "gifted", "graceful", "grand",
	"happy", "handy", "honest", "hopeful", "humble",
	"ideal", "immense", "inspired", "inventive",
	"jazzy", "jolly", "joyful",
	"keen", "kind",
	"logical", "lucid", "loyal", "lively",
	"mature", "mindful", "modern",
	"natural", "nimble", "noble",
	"orderly", "open", "optimal",
	"patient", "peaceful", "precise", "proud",
	"quick", "quiet", "quirky",
	"radiant", "rational", "reliable", "robust",
	"serene", "sharp", "simple", "sincere", "smart", "steady", "stellar", "swift",
	"tender", "thorough", "tidy", "true", "trusty",
	"upbeat", "upright", "useful",
	"valid", "vibrant", "vigilant", "vivid",
	"warm", "wise", "witty",
	"xenial",
	"youthful",
	"zealous", "zesty",
	"acute", "affable", "brilliant", "candid", "cogent", "crisp", "deft", "earnest",
	"eloquent", "fair", "fervent", "flexible", "forthright", "genial", "harmonic",
	"incisive", "ingenious", "inquisitive", "intuitive", "judicious", "knowledgeable",
	"lateral", "methodical", "meticulous", "original", "perceptive", "precise",
	"pragmatic", "principled", "proactive", "resolute", "rigorous", "sagacious",
	"scrupulous", "skilled", "stalwart", "stoic", "strategic", "succinct",
	"tactful", "tenacious", "tireless", "transparent", "versatile", "vibrant",
}

// nouns is the list of scientist, mathematician, and engineer names used in name generation.
var nouns = []string{
	"abel", "ampere", "archimedes", "aristotle", "avogadro",
	"babbage", "baird", "banach", "becquerel", "bell", "bernoulli", "bohr", "boltzmann",
	"boole", "born", "boyle", "bragg", "brahe", "brunel",
	"cantor", "cauchy", "cayley", "celsius", "chadwick", "chebyshev", "compton",
	"copernicus", "coulomb", "crick", "curie",
	"dalton", "darwin", "davy", "debroglie", "dedekind", "dijkstra", "dirac", "doppler",
	"edison", "einstein", "erdos", "euclid", "euler",
	"faraday", "fermat", "fermi", "feynman", "fibonacci", "fleming", "fourier", "franklin",
	"galileo", "galois", "gauss", "germain", "goedel",
	"hamilton", "hawking", "heisenberg", "herschel", "hertz", "hilbert", "hooke", "hopper", "hubble", "huxley",
	"jacobi",
	"kelvin", "kepler", "kernighan", "knuth", "koch",
	"lagrange", "landau", "laplace", "lavoisier", "leibniz", "lie", "linnaeus", "lorentz", "lovelace",
	"markov", "maxwell", "mendel", "mendeleev", "minkowski", "morse",
	"navier", "newton", "noether",
	"ohm", "oppenheimer",
	"pascal", "pasteur", "pauli", "planck", "poincare", "ptolemy", "pythagoras",
	"ramanujan", "riemann", "ritchie", "rontgen", "rutherford",
	"sanger", "schrodinger", "shannon", "stephenson", "stokes", "sylvester",
	"tarski", "tesla", "thales", "thomson", "torricelli", "townes", "turing",
	"vonneumann", "volta",
	"watson", "watt", "weierstrass", "wiener", "wright",
	"yukawa",
	"zuse",
}

// Generate returns a random "adjective_noun" name using the provided random source.
func Generate(rng *rand.Rand) string {
	adj := adjectives[rng.IntN(len(adjectives))]
	noun := nouns[rng.IntN(len(nouns))]
	return adj + "_" + noun
}
